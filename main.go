package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-telegram-bot-api/telegram-bot-api/v5"
	corev1 "k8s.io/api/core/v1"
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

	// 初始化 Telegram Bot
	bot, err := tgbotapi.NewBotAPI(telegramToken)
	if err != nil {
		log.Fatalf("Failed to initialize Telegram Bot: %v", err)
	}
	bot.Debug = true // 可选：启用调试日志
	log.Printf("Authorized on Telegram account %s", bot.Self.UserName)

	// 获取 Kubernetes Clientset（支持 EKS 外部访问）
	clientset, err := getKubeClient()
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// 创建 Shared Informer Factory（监控所有命名空间，resync 周期 30 分钟）
	factory := informers.NewSharedInformerFactory(clientset, 30*time.Minute)

	// 设置 Events Informer 用于采集事件信息
	eventInformer := factory.Core().V1().Events().Informer()
	eventInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			event := obj.(*corev1.Event)
			log.Printf("Event Added: Namespace=%s, InvolvedObject=%s, Reason=%s, Message=%s", event.Namespace, event.InvolvedObject.Name, event.Reason, event.Message)
			// 可扩展：存储到数据库或进一步分析
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldEvent := oldObj.(*corev1.Event)
			newEvent := newObj.(*corev1.Event)
			log.Printf("Event Updated: Namespace=%s, InvolvedObject=%s, Reason=%s -> %s, Message=%s", newEvent.Namespace, newEvent.InvolvedObject.Name, oldEvent.Reason, newEvent.Reason, newEvent.Message)
		},
		DeleteFunc: func(obj interface{}) {
			event := obj.(*corev1.Event)
			log.Printf("Event Deleted: Namespace=%s, InvolvedObject=%s, Reason=%s", event.Namespace, event.InvolvedObject.Name, event.Reason)
		},
	})

	// 设置 Pods Informer 用于监控 Pod 状态并报警
	podInformer := factory.Core().V1().Pods().Informer()
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			log.Printf("Pod Added: %s/%s", pod.Namespace, pod.Name)
			checkAndAlertPodStatus(bot, pod)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			newPod := newObj.(*corev1.Pod)
			log.Printf("Pod Updated: %s/%s, Phase: %s -> %s", newPod.Namespace, newPod.Name, oldPod.Status.Phase, newPod.Status.Phase)
			checkAndAlertPodStatus(bot, newPod)
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			log.Printf("Pod Deleted: %s/%s", pod.Namespace, pod.Name)
		},
	})

	// 启动 Informers
	stopCh := make(chan struct{})
	defer close(stopCh)
	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)

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

// 检查 Pod 状态并发送 Telegram 报警
func checkAndAlertPodStatus(bot *tgbotapi.BotAPI, pod *corev1.Pod) {
	alertMsg := ""
	switch pod.Status.Phase {
	case corev1.PodFailed, corev1.PodUnknown:
		alertMsg = fmt.Sprintf("Pod Alert: %s/%s is in %s phase! Reason: %s", pod.Namespace, pod.Name, pod.Status.Phase, pod.Status.Reason)
	default:
		// 检查 Pod 条件中的错误
		for _, cond := range pod.Status.Conditions {
			if cond.Status == corev1.ConditionFalse && cond.Type == corev1.PodReady {
				alertMsg = fmt.Sprintf("Pod Alert: %s/%s is not ready! Reason: %s, Message: %s", pod.Namespace, pod.Name, cond.Reason, cond.Message)
				break
			}
		}
	}

	if alertMsg != "" {
		msg := tgbotapi.NewMessage(telegramChatID, alertMsg)
		_, err := bot.Send(msg)
		if err != nil {
			log.Printf("Failed to send Telegram alert: %v", err)
		} else {
			log.Printf("Telegram alert sent: %s", alertMsg)
		}
	}
}
