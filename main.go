package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	telegramToken  string
	telegramChatID int64
	clusterName    string
)

// EventCacheEntry stores events for a pod, grouped by reason, and the last alert time
type EventCacheEntry struct {
	Events        map[string][]corev1.Event // Map of reason to events
	LastAlertTime time.Time
}

// eventCache maps pod key (namespace/name) to its events
var (
	eventCache     = make(map[string]*EventCacheEntry)
	cacheMutex     = sync.Mutex{}
	alertThreshold = time.Minute * 5 // Time window for merging events and rate-limiting alerts
)

func main() {
	// 解析命令行参数（或使用环境变量）
	flag.StringVar(&telegramToken, "telegram-token", os.Getenv("TELEGRAM_TOKEN"), "Telegram Bot Token")
	flag.Int64Var(&telegramChatID, "telegram-chat-id", 0, "Telegram Chat ID (use env TELEGRAM_CHAT_ID if not set)")
	flag.Parse()

	if telegramToken == "" {
		log.Fatal("Telegram Bot Token is required")
	}
	if telegramChatID == 0 {
		chatIDStr := os.Getenv("TELEGRAM_CHAT_ID")
		if chatIDStr == "" {
			log.Fatal("Telegram Chat ID is required")
		}
		fmt.Sscanf(chatIDStr, "%d", &telegramChatID)
	}

	// 获取 Kubernetes Clientset（支持 EKS 外部访问）
	clientset, err := getKubeClient()
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// 从 ConfigMap 读取 cluster_name
	clusterName, err = loadClusterNameFromConfigMap(clientset, "default", "pod-monitor-config")
	if err != nil {
		log.Fatalf("Failed to load cluster name from ConfigMap: %v", err)
	}
	log.Printf("Loaded cluster name: %s", clusterName)

	// 初始化 Telegram Bot
	bot, err := tgbotapi.NewBotAPI(telegramToken)
	if err != nil {
		log.Fatalf("Failed to initialize Telegram Bot: %v", err)
	}
	bot.Debug = true // 可选：启用调试日志
	log.Printf("Authorized on Telegram account %s", bot.Self.UserName)

	// 创建 Shared Informer Factory（监控所有命名空间，resync 周期 30 分钟）
	factory := informers.NewSharedInformerFactory(clientset, 30*time.Minute)

	// 设置 Events Informer 用于采集事件信息
	eventInformer := factory.Core().V1().Events().Informer()
	eventInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			event := obj.(*corev1.Event)
			if event.InvolvedObject.Kind == "Pod" && event.Type == corev1.EventTypeWarning {
				log.Printf("Event Added: Namespace=%s, Pod=%s, Type=%s, Reason=%s, Message=%s", event.Namespace, event.InvolvedObject.Name, event.Type, event.Reason, event.Message)
				checkAndAlertPodStatus(bot, clientset, event)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newEvent := newObj.(*corev1.Event)
			if newEvent.InvolvedObject.Kind == "Pod" && newEvent.Type == corev1.EventTypeWarning {
				log.Printf("Event Updated: Namespace=%s, Pod=%s, Type=%s, Reason=%s, Message=%s", newEvent.Namespace, newEvent.InvolvedObject.Name, newEvent.Type, newEvent.Reason, newEvent.Message)
				checkAndAlertPodStatus(bot, clientset, newEvent)
			}
		},
		DeleteFunc: func(obj interface{}) {
			event := obj.(*corev1.Event)
			if event.InvolvedObject.Kind == "Pod" && event.Type == corev1.EventTypeWarning {
				log.Printf("Event Deleted: Namespace=%s, Pod=%s, Type=%s, Reason=%s", event.Namespace, event.InvolvedObject.Name, event.Type, event.Reason)
				// Clean up cache on event deletion
				cacheMutex.Lock()
				delete(eventCache, fmt.Sprintf("%s/%s", event.Namespace, event.InvolvedObject.Name))
				cacheMutex.Unlock()
			}
		},
	})

	// 启动 Informers
	stopCh := make(chan struct{})
	defer close(stopCh)
	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)

	// 定期清理旧事件
	go cleanupEventCache()

	// 监听信号以优雅退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Shutting down...")
}

// 获取 Kubernetes Clientset（EKS 外部访问使用 kubeconfig）
func getKubeClient() (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	// 尝试 in-cluster 配置（如果在集群内运行）
	config, err = rest.InClusterConfig()
	if err != nil {
		// 回退到 out-of-cluster 配置（使用 ~/.kube/config）
		kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to build config: %w", err)
		}
		log.Println("Using out-of-cluster configuration (kubeconfig)")
	} else {
		log.Println("Using in-cluster configuration")
	}

	return kubernetes.NewForConfig(config)
}

// 从 ConfigMap 读取 cluster_name
func loadClusterNameFromConfigMap(clientset *kubernetes.Clientset, namespace, name string) (string, error) {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get ConfigMap %s/%s: %w", namespace, name, err)
	}
	clusterName, ok := cm.Data["cluster_name"]
	if !ok || clusterName == "" {
		return "", fmt.Errorf("cluster_name not found in ConfigMap %s/%s", namespace, name)
	}
	return clusterName, nil
}

// 清理过时的事件缓存
func cleanupEventCache() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cacheMutex.Lock()
		for podKey, entry := range eventCache {
			if time.Since(entry.LastAlertTime) > alertThreshold {
				delete(eventCache, podKey)
			}
		}
		cacheMutex.Unlock()
	}
}

// 转义 MarkdownV2 特殊字符
func escapeMarkdownV2(text string) string {
	specialChars := []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, char := range specialChars {
		text = strings.ReplaceAll(text, char, "\\"+char)
	}
	return text
}

// 检查 Pod 是否未就绪
func isPodNotReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionFalse {
			return true
		}
	}
	return false
}

// 获取 Pod 日志（最后 5 行，最大 1000 字符）
func getPodLogs(clientset *kubernetes.Clientset, namespace, podName string) (string, error) {
	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		TailLines: int64Ptr(5), // 获取最后 5 行
	})
	logs, err := req.Stream(context.Background())
	if err != nil {
		return "", fmt.Errorf("failed to get logs for pod %s/%s: %w", namespace, podName, err)
	}
	defer logs.Close()

	var buf bytes.Buffer
	_, err = io.Copy(&buf, logs)
	if err != nil {
		return "", fmt.Errorf("failed to read logs for pod %s/%s: %w", namespace, podName, err)
	}

	logContent := buf.String()
	if logContent == "" {
		return "No logs available", nil
	}

	// 截断日志以避免超过 Telegram 消息限制（4096 字符）
	if len(logContent) > 1000 {
		logContent = logContent[:1000] + "... [truncated]"
	}

	// 按行分割并取最后 5 行
	lines := strings.Split(strings.TrimSpace(logContent), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	logContent = strings.Join(lines, "\n")

	return logContent, nil
}

// 辅助函数：将 int64 转换为指针
func int64Ptr(i int64) *int64 {
	return &i
}

// 辅助函数：检查切片中是否包含指定字符串
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// 检查 Pod 状态并发送 Telegram 报警
func checkAndAlertPodStatus(bot *tgbotapi.BotAPI, clientset *kubernetes.Clientset, event *corev1.Event) {
	if event.InvolvedObject.Kind != "Pod" || event.Type != corev1.EventTypeWarning {
		return // 只处理 Pod 相关的 Warning 事件
	}

	podKey := fmt.Sprintf("%s/%s", event.Namespace, event.InvolvedObject.Name)

	// 获取 Pod 信息以检查状态和提取标签
	pod, err := clientset.CoreV1().Pods(event.Namespace).Get(context.Background(), event.InvolvedObject.Name, metav1.GetOptions{})
	if err != nil {
		log.Printf("Failed to get pod %s: %v", podKey, err)
		return
	}

	// 只处理未就绪的 Pod
	if !isPodNotReady(pod) {
		return
	}

	// 更新事件缓存，按 Reason 分组
	cacheMutex.Lock()
	entry, exists := eventCache[podKey]
	if !exists {
		entry = &EventCacheEntry{Events: make(map[string][]corev1.Event)}
		eventCache[podKey] = entry
	}
	entry.Events[event.Reason] = append(entry.Events[event.Reason], *event)
	cacheMutex.Unlock()

	// 检查是否需要发送报警（基于时间窗口）
	if time.Since(entry.LastAlertTime) < alertThreshold {
		return // 避免重复报警
	}

	// 获取 Pod 日志
	logContent, err := getPodLogs(clientset, event.Namespace, event.InvolvedObject.Name)
	if err != nil {
		log.Printf("Failed to fetch logs for %s: %v", podKey, err)
		logContent = "Failed to fetch logs"
	}

	// 构建 Markdown 格式的报警消息
	serviceName := pod.Labels["app"]
	if serviceName == "" {
		serviceName = pod.Labels["k8s-app"]
	}
	if serviceName == "" {
		serviceName = "Unknown"
	}
	currentTime := time.Now().Format("2006-01-02 15:04:05 MST")

	// 合并事件信息，包含事件计数
	var reasonSummaries []string
	var messageSummaries []string
	for reason, events := range entry.Events {
		count := len(events)
		var messages []string
		for _, e := range events {
			if !contains(messages, e.Message) {
				messages = append(messages, e.Message)
			}
		}
		reasonSummaries = append(reasonSummaries, fmt.Sprintf("%s (%d times)", reason, count))
		messageSummaries = append(messageSummaries, strings.Join(messages, "; "))
	}
	reasonSummary := strings.Join(reasonSummaries, ", ")
	messageSummary := strings.Join(messageSummaries, "; ")

	// 转义 MarkdownV2 特殊字符
	escapedClusterName := escapeMarkdownV2(clusterName)
	escapedPodName := escapeMarkdownV2(event.InvolvedObject.Name)
	escapedNamespace := escapeMarkdownV2(event.Namespace)
	escapedServiceName := escapeMarkdownV2(serviceName)
	escapedCurrentTime := escapeMarkdownV2(currentTime)
	escapedReasonSummary := escapeMarkdownV2(reasonSummary)
	escapedMessageSummary := escapeMarkdownV2(messageSummary)
	escapedLogContent := escapeMarkdownV2(logContent)

	// 优化 MarkdownV2 格式，使用 emoji 和加粗，添加日志
	alertMsg := fmt.Sprintf(
		"⚠️ *Pod Alert*\n"+
			"**🖥️ Cluster**: %s\n"+
			"**📛 Pod Name**: %s\n"+
			"**🌐 Namespace**: %s\n"+
			"**🔧 Service**: %s\n"+
			"**⏰ Time**: %s\n"+
			"**⚠️ Status**: Not Ready\n"+
			"**❗ Reasons**: %s\n"+
			"**💬 Messages**: %s\n"+
			"**📜 Logs**:\n```%s```",
		escapedClusterName, escapedPodName, escapedNamespace, escapedServiceName, escapedCurrentTime, escapedReasonSummary, escapedMessageSummary, escapedLogContent)

	// 检查消息长度（Telegram 限制 4096 字符）
	if len(alertMsg) > 4096 {
		log.Printf("Alert message too long (%d characters), truncating logs", len(alertMsg))
		escapedLogContent = escapeMarkdownV2(logContent[:500] + "... [truncated]")
		alertMsg = fmt.Sprintf(
			"⚠️ *Pod Alert*\n"+
				"**🖥️ Cluster**: %s\n"+
				"**📛 Pod Name**: %s\n"+
				"**🌐 Namespace**: %s\n"+
				"**🔧 Service**: %s\n"+
				"**⏰ Time**: %s\n"+
				"**⚠️ Status**: Not Ready\n"+
				"**❗ Reasons**: %s\n"+
				"**💬 Messages**: %s\n"+
				"**📜 Logs**:\n```%s```",
			escapedClusterName, escapedPodName, escapedNamespace, escapedServiceName, escapedCurrentTime, escapedReasonSummary, escapedMessageSummary, escapedLogContent)
	}

	// 发送 Telegram 报警
	msg := tgbotapi.NewMessage(telegramChatID, alertMsg)
	msg.ParseMode = "MarkdownV2"
	_, err = bot.Send(msg)
	if err != nil {
		log.Printf("Failed to send Telegram alert: %v", err)
	} else {
		log.Printf("Telegram alert sent: %s", alertMsg)
		cacheMutex.Lock()
		entry.LastAlertTime = time.Now()
		cacheMutex.Unlock()
	}
}
