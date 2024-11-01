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
				logger.Errorf("AsyncWriter failed to write data: %v", err)
			}
		}
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
	prometheus.MustRegister(errorCount)
	prometheus.MustRegister(latencyMetric)
	logger.Info("Prometheus metrics initialized")
}

func initTracing() {
	logger.Debug("Initializing tracing...")

	ctx := context.Background()

	alloyEndpoint := os.Getenv("ALLOY_LOG_ENDPOINT")
	if alloyEndpoint == "" {
		alloyEndpoint = "http://grafana-alloy.georgetestapp.svc.cluster.local:12345"
	}
	logger.Debugf("Using Alloy endpoint for traces: %s", alloyEndpoint)

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(alloyEndpoint),
		otlptracehttp.WithInsecure(),
		otlptracehttp.WithTimeout(20*time.Second),
	)
	if err != nil {
		logger.Fatalf("Failed to create trace exporter: %v", err)
	}

	attrs := []attribute.KeyValue{
		attribute.String("service.name", "georgetestapp"),
		attribute.String("environment", "test"),
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			attrs...,
		)),
	)

	otel.SetTracerProvider(tp)
	tracer = tp.Tracer("georgetestapp")

	logger.Debug("Tracing initialized successfully")
}

func main() {
	logger.SetLevel(logrus.DebugLevel)

	logger.Info("Starting initialization...")
	initMetrics()
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
	http.HandleFunc("/", handleRequest)
	http.Handle("/metrics", promhttp.Handler())

	go simulateTraffic()
	go continuousLogTraceToLokiAndTempo()

	logger.Info("Starting server on port 8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}

// Helper to extract traceID from context
func getTraceID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if span != nil {
		return span.SpanContext().TraceID().String()
	}
	return ""
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handleRequest")
	defer span.End()

	traceID := getTraceID(ctx)

	latency := time.Duration(rand.Intn(300)) * time.Millisecond
	time.Sleep(latency)

	requestCount.Inc()
	latencyMetric.Observe(latency.Seconds())

	userID := r.Header.Get("UserID")
	if userID == "" {
		userID = "anonymous"
	}
	span.SetAttributes(attribute.String("userId", userID))

	logger.WithFields(logrus.Fields{
		"endpoint":  "/",
		"userId":    userID,
		"latency":   latency.Seconds(),
		"requestID": strconv.Itoa(rand.Intn(1000000)),
		"traceID":   traceID,
	}).Info("Request handled successfully")
}

func simulateTraffic() {
	for {
		for i := 0; i < 10; i++ {
			go simulateRequest(false)
		}
		time.Sleep(1 * time.Second)
		for j := 0; j < 3; j++ {
			go simulateRequest(true)
			time.Sleep(1 * time.Second)
		}
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

	traceID := getTraceID(req.Context())

	if causeError || err != nil || (resp != nil && resp.StatusCode >= 400) {
		errorCount.Inc()
		trafficLogger.WithFields(logrus.Fields{
			"endpoint": "/",
			"userId":   req.Header.Get("UserID"),
			"status":   "error",
			"traceID":  traceID,
			"reason":   err,
		}).Error("Simulated error occurred")
	} else {
		trafficLogger.WithFields(logrus.Fields{
			"endpoint": "/",
			"userId":   req.Header.Get("UserID"),
			"status":   "success",
			"traceID":  traceID,
		}).Info("Simulated successful request")
	}
}

// continuousLogTraceToLokiAndTempo sends associated logs and traces to Loki and Tempo
// continuousLogTraceToLokiAndTempo sends associated logs and traces to Loki and Tempo
func continuousLogTraceToLokiAndTempo() {
	tempoURL := "http://singletempo.monitoring.svc.cluster.local:4318/v1/traces"
	lokiURL := "http://loki-gateway.monitoring.svc.cluster.local/loki/api/v1/push"

	for {
		// Generate trace and span IDs
		traceID := fmt.Sprintf("%032X", rand.Int63())
		rootSpanID := fmt.Sprintf("%016X", rand.Int31())
		childSpanID1 := fmt.Sprintf("%016X", rand.Int31())
		childSpanID2 := fmt.Sprintf("%016X", rand.Int31())

		// Determine status code (1 in 5 is 500, otherwise 200)
		statusCode := 200
		if rand.Intn(5) == 0 {
			statusCode = 500
		}

		// Create multi-layer trace payload for Tempo
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
								// Root span
								{
									"traceId":           traceID,
									"spanId":            rootSpanID,
									"name":              "handleRequest",
									"startTimeUnixNano": time.Now().Add(-3 * time.Second).UnixNano(),
									"endTimeUnixNano":   time.Now().UnixNano(),
									"kind":              1, // SERVER
									"attributes": []map[string]interface{}{
										{
											"key": "service.name",
											"value": map[string]interface{}{
												"stringValue": "georgetestapp",
											},
										},
										{
											"key": "http.status_code",
											"value": map[string]interface{}{
												"intValue": statusCode,
											},
										},
									},
								},
								// First child span
								{
									"traceId":           traceID,
									"spanId":            childSpanID1,
									"parentSpanId":      rootSpanID,
									"name":              "processRequest",
									"startTimeUnixNano": time.Now().Add(-2 * time.Second).UnixNano(),
									"endTimeUnixNano":   time.Now().Add(-1 * time.Second).UnixNano(),
									"kind":              2, // INTERNAL
									"attributes": []map[string]interface{}{
										{
											"key": "task",
											"value": map[string]interface{}{
												"stringValue": "processRequest",
											},
										},
										{
											"key": "http.status_code",
											"value": map[string]interface{}{
												"intValue": statusCode,
											},
										},
									},
								},
								// Second child span
								{
									"traceId":           traceID,
									"spanId":            childSpanID2,
									"parentSpanId":      childSpanID1,
									"name":              "subProcess1",
									"startTimeUnixNano": time.Now().Add(-1 * time.Second).UnixNano(),
									"endTimeUnixNano":   time.Now().Add(-500 * time.Millisecond).UnixNano(),
									"kind":              2, // INTERNAL
									"attributes": []map[string]interface{}{
										{
											"key": "task",
											"value": map[string]interface{}{
												"stringValue": "subProcess1",
											},
										},
										{
											"key": "http.status_code",
											"value": map[string]interface{}{
												"intValue": statusCode,
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

		// Create log payload for Loki with associated trace ID, service name, and status code
		logEntry := map[string]interface{}{
			"streams": []map[string]interface{}{
				{
					"stream": map[string]string{
						"service_name": "georgetestapp",
						"status_code":  strconv.Itoa(statusCode), // Include status code in logs
					},
					"values": [][]string{
						{
							fmt.Sprintf("%d", time.Now().UnixNano()),
							fmt.Sprintf("Example log message from georgetestapp with traceID=%s | X-B3-TraceId: %s | Status: %d", traceID, traceID, statusCode),
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
