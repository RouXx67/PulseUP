package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rcourtman/pulse-go-rewrite/internal/dockeragent"
	"github.com/rs/zerolog"
)

type targetFlagList []string

func (l *targetFlagList) String() string {
	return strings.Join(*l, ",")
}

func (l *targetFlagList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func main() {
	cfg := loadConfig()

	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
	cfg.Logger = &logger

	agent, err := dockeragent.New(cfg)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to create docker agent")
	}
	defer agent.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info().Str("pulse_url", cfg.PulseURL).Dur("interval", cfg.Interval).Msg("Starting Pulse Docker agent")

	if err := agent.Run(ctx); err != nil && err != context.Canceled {
		logger.Fatal().Err(err).Msg("Agent terminated with error")
	}

	logger.Info().Msg("Agent stopped")
}

func loadConfig() dockeragent.Config {
	envURL := strings.TrimSpace(os.Getenv("PULSE_URL"))
	envToken := strings.TrimSpace(os.Getenv("PULSE_TOKEN"))
	envInterval := strings.TrimSpace(os.Getenv("PULSE_INTERVAL"))
	envHostname := strings.TrimSpace(os.Getenv("PULSE_HOSTNAME"))
	envAgentID := strings.TrimSpace(os.Getenv("PULSE_AGENT_ID"))
	envInsecure := strings.TrimSpace(os.Getenv("PULSE_INSECURE_SKIP_VERIFY"))
	envNoAutoUpdate := strings.TrimSpace(os.Getenv("PULSE_NO_AUTO_UPDATE"))
	envTargets := strings.TrimSpace(os.Getenv("PULSE_TARGETS"))

	defaultInterval := 30 * time.Second
	if envInterval != "" {
		if parsed, err := time.ParseDuration(envInterval); err == nil {
			defaultInterval = parsed
		}
	}

	urlFlag := flag.String("url", envURL, "Pulse server URL (e.g. http://pulse:7655)")
	tokenFlag := flag.String("token", envToken, "Pulse API token (required)")
	intervalFlag := flag.Duration("interval", defaultInterval, "Reporting interval (e.g. 30s)")
	hostnameFlag := flag.String("hostname", envHostname, "Override hostname reported to Pulse")
	agentIDFlag := flag.String("agent-id", envAgentID, "Override agent identifier")
	insecureFlag := flag.Bool("insecure", parseBool(envInsecure), "Skip TLS certificate verification")
	noAutoUpdateFlag := flag.Bool("no-auto-update", parseBool(envNoAutoUpdate), "Disable automatic agent updates")
	var targetFlags targetFlagList
	flag.Var(&targetFlags, "target", "Pulse target in url|token[|insecure] format. Repeat to send to multiple Pulse instances")

	flag.Parse()

	pulseURL := *urlFlag
	if pulseURL == "" {
		pulseURL = "http://localhost:7655"
	}

	targets := make([]dockeragent.TargetConfig, 0)

	if len(targetFlags) > 0 {
		parsedTargets, err := parseTargetSpecs(targetFlags)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		targets = append(targets, parsedTargets...)
	}

	if envTargets != "" {
		envTargetSpecs := splitTargetSpecs(envTargets)
		if len(envTargetSpecs) > 0 {
			parsedTargets, err := parseTargetSpecs(envTargetSpecs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			targets = append(targets, parsedTargets...)
		}
	}

	token := strings.TrimSpace(*tokenFlag)
	if token == "" && len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "error: PULSE_TOKEN, --token, or at least one --target/PULSE_TARGETS entry must be provided")
		flag.Usage()
		os.Exit(1)
	}

	interval := *intervalFlag
	if interval <= 0 {
		interval = 30 * time.Second
	}

	return dockeragent.Config{
		PulseURL:           pulseURL,
		APIToken:           token,
		Interval:           interval,
		HostnameOverride:   strings.TrimSpace(*hostnameFlag),
		AgentID:            strings.TrimSpace(*agentIDFlag),
		InsecureSkipVerify: *insecureFlag,
		DisableAutoUpdate:  *noAutoUpdateFlag,
		Targets:            targets,
	}
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func parseTargetSpecs(specs []string) ([]dockeragent.TargetConfig, error) {
	targets := make([]dockeragent.TargetConfig, 0, len(specs))
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		target, err := parseTargetSpec(spec)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func parseTargetSpec(spec string) (dockeragent.TargetConfig, error) {
	parts := strings.Split(spec, "|")
	if len(parts) < 2 {
		return dockeragent.TargetConfig{}, fmt.Errorf("invalid target %q: expected format url|token[|insecure]", spec)
	}

	url := strings.TrimSpace(parts[0])
	token := strings.TrimSpace(parts[1])
	if url == "" {
		return dockeragent.TargetConfig{}, fmt.Errorf("invalid target %q: URL is required", spec)
	}
	if token == "" {
		return dockeragent.TargetConfig{}, fmt.Errorf("invalid target %q: token is required", spec)
	}

	insecure := false
	if len(parts) >= 3 {
		switch strings.ToLower(strings.TrimSpace(parts[2])) {
		case "1", "true", "yes", "y", "on":
			insecure = true
		case "", "0", "false", "no", "n", "off":
			insecure = false
		default:
			return dockeragent.TargetConfig{}, fmt.Errorf("invalid target %q: insecure flag must be true/false", spec)
		}
	}

	return dockeragent.TargetConfig{
		URL:                url,
		Token:              token,
		InsecureSkipVerify: insecure,
	}, nil
}

func splitTargetSpecs(value string) []string {
	if value == "" {
		return nil
	}

	normalized := strings.ReplaceAll(value, "\n", ";")
	raw := strings.Split(normalized, ";")
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
