package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	requestCount  = prometheus.NewCounter(prometheus.CounterOpts{Name: "app_requests_total", Help: "Total number of requests"})
	errorCount    = prometheus.NewCounter(prometheus.CounterOpts{Name: "app_errors_total", Help: "Total number of errors"})
	latencyMetric = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "app_latency_seconds",
		Help:    "Latency of HTTP requests",
		Buckets: prometheus.DefBuckets,
	})

	logger        = logrus.New() // Initialize logrus logger
	tracer        trace.Tracer   // OpenTelemetry tracer
	trafficLogger *logrus.Logger // Separate logger for simulated traffic
	asyncWriter   *AsyncWriter   // Asynchronous writer for traffic logs
)

// AsyncWriter handles asynchronous writing to a file using a buffered channel
type AsyncWriter struct {
	ch   chan []byte
	done chan struct{}
}

// NewAsyncWriter creates a new AsyncWriter
func NewAsyncWriter(filePath string, bufferSize int) (*AsyncWriter, error) {
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}

	aw := &AsyncWriter{
		ch:   make(chan []byte, bufferSize),
		done: make(chan struct{}),
	}

	go func() {
		writer := bufio.NewWriter(file)
		for data := range aw.ch {
			_, err := writer.Write(data)
			if err != nil {
				// Handle write error, e.g., log to main logger
				logger.Errorf("AsyncWriter failed to write data: %v", err)
			}
		}
		// Flush any remaining data
		if err := writer.Flush(); err != nil {
			logger.Errorf("AsyncWriter failed to flush data: %v", err)
		}
		file.Close()
		close(aw.done)
	}()

	return aw, nil
}

// Write sends data to the AsyncWriter's channel
func (aw *AsyncWriter) Write(p []byte) (n int, err error) {
	select {
	case aw.ch <- p:
		return len(p), nil
	default:
		// Buffer is full; drop the log or handle accordingly
		return 0, fmt.Errorf("AsyncWriter buffer is full")
	}
}

// Close gracefully shuts down the AsyncWriter
func (aw *AsyncWriter) Close() {
	close(aw.ch)
	<-aw.done
}

func initMetrics() {
	logger.Debug("Initializing Prometheus metrics...")
	prometheus.MustRegister(requestCount)
	logger.Debug("Registered requestCount metric")
	prometheus.MustRegister(errorCount)
	logger.Debug("Registered errorCount metric")
	prometheus.MustRegister(latencyMetric)
	logger.Debug("Registered latencyMetric")

	logger.Info("Prometheus metrics initialized")
}

func initTracing() {
	logger.Debug("Initializing tracing...")

	ctx := context.Background()

	// Use the ALLOY_LOG_ENDPOINT environment variable with a default value
	alloyEndpoint := os.Getenv("ALLOY_LOG_ENDPOINT")
	if alloyEndpoint == "" {
		alloyEndpoint = "http://grafana-alloy.georgetestapp.svc.cluster.local:12345"
	}
	logger.Debugf("Using Alloy endpoint for traces: %s", alloyEndpoint)

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(alloyEndpoint),
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithTimeout(20*time.Second), // Add a timeout to ensure delivery
	)
	if err != nil {
		logger.Fatalf("Failed to create trace exporter: %v", err)
	}

	// Resource attributes to identify the service
	attrs := []attribute.KeyValue{
		attribute.String("service.name", "georgetestapp"),
		attribute.String("environment", "test"),
	}

	// Create TracerProvider with the configured exporter
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()), // Sample all traces for testing
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			attrs...,
		)),
	)

	// Set the TracerProvider globally
	otel.SetTracerProvider(tp)
	tracer = tp.Tracer("georgetestapp")

	logger.Debug("Tracing initialized successfully")
}

func main() {
	logger.SetLevel(logrus.DebugLevel)

	logger.Info("Starting initialization...")
	initMetrics()
	logger.Debug("Metrics initialized successfully")

	// Initialize traffic logger
	trafficLogger = logrus.New()
	trafficLogger.SetFormatter(&logrus.JSONFormatter{})
	trafficLogger.SetLevel(logrus.InfoLevel)

	var err error
	asyncWriter, err = NewAsyncWriter("/var/log/simulated_traffic.log", 1000)
	if err != nil {
		logger.Fatalf("Failed to initialize traffic logger: %v", err)
	}
	defer asyncWriter.Close()

	trafficLogger.SetOutput(asyncWriter)

	initTracing()
	logger.Debug("Tracing initialized successfully")

	http.HandleFunc("/", handleRequest)
	http.Handle("/metrics", promhttp.Handler())

	go simulateTraffic()
	go continuousLogTraceToLokiAndTempo() // Start continuous logging and tracing with association
	logger.Debug("Traffic simulation and continuous logging/tracing to Loki and Tempo started")

	logger.Info("Starting server on port 8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	_, span := tracer.Start(r.Context(), "handleRequest")
	defer span.End()

	// Simulate latency
	latency := time.Duration(rand.Intn(300)) * time.Millisecond
	time.Sleep(latency)

	requestCount.Inc()
	latencyMetric.Observe(latency.Seconds())

	userID := r.Header.Get("UserID")
	if userID == "" {
		userID = "anonymous"
	}
	span.SetAttributes(attribute.String("userId", userID))

	// Debugging Information
	logger.WithFields(logrus.Fields{
		"endpoint":  "/",
		"userId":    userID,
		"latency":   latency.Seconds(),
		"requestID": strconv.Itoa(rand.Intn(1000000)),
	}).Info("Request handled successfully")
}

func simulateTraffic() {
	for {
		// Stable traffic: 10 requests per second
		for i := 0; i < 10; i++ {
			go simulateRequest(false)
		}
		time.Sleep(1 * time.Second)

		// Error burst for 3 seconds
		for j := 0; j < 3; j++ {
			go simulateRequest(true)
			time.Sleep(1 * time.Second)
		}

		// Latency spike for 5 seconds
		for k := 0; k < 5; k++ {
			go simulateRequest(false)
			time.Sleep(1 * time.Second)
		}
	}
}

func simulateRequest(causeError bool) {
	req, _ := http.NewRequest("GET", "http://localhost:8080", nil)
	req.Header.Set("UserID", strconv.Itoa(rand.Intn(1000)))

	client := &http.Client{}
	resp, err := client.Do(req)

	// Log request info to the traffic logger
	if causeError || err != nil || (resp != nil && resp.StatusCode >= 400) {
		errorCount.Inc()
		trafficLogger.WithFields(logrus.Fields{
			"endpoint": "/",
			"userId":   req.Header.Get("UserID"),
			"status":   "error",
			"reason":   err,
		}).Error("Simulated error occurred")
	} else {
		trafficLogger.WithFields(logrus.Fields{
			"endpoint": "/",
			"userId":   req.Header.Get("UserID"),
			"status":   "success",
		}).Info("Simulated successful request")
	}
}

// continuousLogTraceToLokiAndTempo sends associated logs and traces to Loki and Tempo
func continuousLogTraceToLokiAndTempo() {
	tempoURL := "http://singletempo.monitoring.svc.cluster.local:4318/v1/traces"
	lokiURL := "http://loki-gateway.monitoring.svc.cluster.local/loki/api/v1/push"

	for {
		// Generate trace and span IDs
		traceID := fmt.Sprintf("%032X", rand.Int63())
		spanID := fmt.Sprintf("%016X", rand.Int31())

		// Create trace payload for Tempo
		tracePayload := map[string]interface{}{
			"resourceSpans": []map[string]interface{}{
				{
					"resource": map[string]interface{}{
						"attributes": []map[string]interface{}{
							{
								"key": "service.name",
								"value": map[string]interface{}{
									"stringValue": "georgetestapp",
								},
							},
						},
					},
					"scopeSpans": []map[string]interface{}{
						{
							"scope": map[string]interface{}{
								"name":    "georgetestapp.library",
								"version": "1.0.0",
							},
							"spans": []map[string]interface{}{
								{
									"traceId":           traceID,
									"spanId":            spanID,
									"name":              "Continuous Span",
									"startTimeUnixNano": time.Now().Add(-1 * time.Second).UnixNano(),
									"endTimeUnixNano":   time.Now().UnixNano(),
									"kind":              2,
									"attributes": []map[string]interface{}{
										{
											"key": "custom.span.attribute",
											"value": map[string]interface{}{
												"stringValue": "Example span attribute",
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}

		// Send trace to Tempo
		sendJSON(tracePayload, tempoURL)

		// Create log payload for Loki with associated trace ID and service name label
		logEntry := map[string]interface{}{
			"streams": []map[string]interface{}{
				{
					"stream": map[string]string{
						"service_name": "georgetestapp", // Consistent label for querying
						"traceID":      traceID,         // Label for trace association
					},
					"values": [][]string{
						{
							fmt.Sprintf("%d", time.Now().UnixNano()),
							fmt.Sprintf("Example log message from georgetestapp with trace ID: %s", traceID),
						},
					},
				},
			},
		}

		// Send log to Loki
		sendJSON(logEntry, lokiURL)

		time.Sleep(5 * time.Second)
	}
}

// Helper function to send JSON payload to a given URL
func sendJSON(data map[string]interface{}, url string) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		logger.Errorf("Failed to marshal data for %s: %v", url, err)
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		logger.Errorf("Failed to create HTTP request for %s: %v", url, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logger.Errorf("Failed to send data to %s: %v", url, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		logger.Errorf("Non-200 response from %s: %s - %s", url, resp.Status, string(responseBody))
	} else {
		logger.Infof("Data sent to %s: %s", url, resp.Status)
	}
}
