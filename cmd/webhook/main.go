package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/webhook"
)

func main() {
	var (
		port          = flag.Int("port", 9443, "Webhook server port")
		certDir       = flag.String("cert-dir", "/etc/webhook/certs", "Directory containing TLS certificates")
		eppURL        = flag.String("epp-url", "http://epp:8080", "EPP endpoint URL")
		namespace     = flag.String("namespace", "default", "Namespace to watch for pods")
	)
	flag.Parse()

	// Setup logging
	opts := ctrlzap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(ctrlzap.New(ctrlzap.UseFlagOptions(&opts)))

	logger := log.Log

	// Create Kubernetes client
	kubeConfig := ctrl.GetConfigOrDie()
	k8sClient, err := client.New(kubeConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		logger.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	// Create EPP client
	eppClient := webhook.NewEPPClient(*eppURL)

	// Create admission handler
	handler := webhook.NewAdmissionHandler(k8sClient, eppClient, *namespace)

	// Create webhook server
	webhookServer := webhook.NewServer(*port, *certDir, handler)

	// Setup graceful shutdown
	stopCh := make(chan struct{})
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		logger.Info("Received shutdown signal")
		close(stopCh)
	}()

	// Start webhook server
	logger.Info("Starting webhook server", "port", *port)
	if err := webhookServer.Start(stopCh); err != nil {
		logger.Error(err, "webhook server failed")
		os.Exit(1)
	}

	logger.Info("Webhook server stopped")
}
