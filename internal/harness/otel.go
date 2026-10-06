package harness

// The OpenTelemetry SDK's standard exporter variables, which several agents read.
const (
	EnvOTLPProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	EnvOTLPEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPHeaders        = "OTEL_EXPORTER_OTLP_HEADERS"
	EnvResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"
)

// ProtocolHTTPProtobuf is chosen over grpc because it traverses HTTPS proxies and TLS interception.
const ProtocolHTTPProtobuf = "http/protobuf"
