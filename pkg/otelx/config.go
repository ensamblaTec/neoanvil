package otelx

// Config is the OpenTelemetry settings block. Lives under
// `nexus.observability.otel:` in nexus.yaml. Default disabled.
//
// When Enabled is true, neo-nexus / neo-mcp expect SetTracer to be
// called by an operator-supplied init function before traffic flows.
// The package itself doesn't know how to talk OTLP — that's the
// real-tracer adapter's job.
//
// [Area 6.1.B]

type Config struct {
	Enabled bool   `yaml:"enabled"`
	Service string `yaml:"service_name"` // resource attribute "service.name"
	Endpoint string `yaml:"endpoint"`     // OTLP endpoint URL (operator-supplied)
	Protocol string `yaml:"protocol"`     // "grpc" | "http/protobuf"
	SampleRate float64 `yaml:"sample_rate"` // 0.0 .. 1.0
}

// Defaults returns a populated Config in the disabled state. Callers
// merge over yaml-loaded values.
func Defaults() Config {
	return Config{
		Enabled:    false,
		Service:    "neoanvil",
		Endpoint:   "",
		Protocol:   "grpc",
		SampleRate: 1.0,
	}
}
