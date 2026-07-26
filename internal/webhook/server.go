package webhook

import (
	"crypto/tls"
	"fmt"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Server manages the webhook HTTPS server.
type Server struct {
	port            int
	certDir         string
	handler         *AdmissionHandler
	tlsConfig       *tls.Config
	server          *http.Server
}

// NewServer creates a new webhook server.
func NewServer(port int, certDir string, handler *AdmissionHandler) *Server {
	return &Server{
		port:    port,
		certDir: certDir,
		handler: handler,
	}
}

// Start starts the webhook server.
func (s *Server) Start(stopCh <-chan struct{}) error {
	logger := log.Log

	// Setup HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", s.handler.Handle)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Load TLS certificates
	certPath := fmt.Sprintf("%s/tls.crt", s.certDir)
	keyPath := fmt.Sprintf("%s/tls.key", s.certDir)

	logger.Info("Starting webhook server", "port", s.port, "certDir", s.certDir)

	s.server = &http.Server{
		Addr:      fmt.Sprintf(":%d", s.port),
		Handler:   mux,
		TLSConfig: &tls.Config{},
	}

	// Start server in a goroutine
	go func() {
		if err := s.server.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "webhook server error")
		}
	}()

	// Wait for stop signal
	<-stopCh
	logger.Info("Stopping webhook server")
	return s.server.Close()
}
