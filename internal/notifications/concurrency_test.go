package notifications

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/RouXx67/PulseUp/internal/alerts"
	"github.com/rs/zerolog"
)

func TestNotificationManagerEmailConfigConcurrency(t *testing.T) {
	origLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.Disabled)
	t.Cleanup(func() {
		zerolog.SetGlobalLevel(origLevel)
	})

	manager := NewNotificationManager("")
	manager.SetGroupingWindow(0)
	manager.SetCooldown(0)

	initialConfig := EmailConfig{
		Enabled:  true,
		SMTPHost: "127.0.0.1",
		SMTPPort: 2525,
		From:     "initial@example.com",
		To:       []string{"initial@example.com"},
	}
	manager.SetEmailConfig(initialConfig)

	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			cfg := EmailConfig{
				Enabled:  true,
				SMTPHost: "127.0.0.1",
				SMTPPort: 2525,
				From:     fmt.Sprintf("sender-%d@example.com", i),
				To:       []string{fmt.Sprintf("recipient-%d@example.com", i)},
			}
			manager.SetEmailConfig(cfg)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			alert := &alerts.Alert{
				ID:           fmt.Sprintf("alert-%d", i),
				Type:         "cpu",
				Level:        alerts.AlertLevelWarning,
				ResourceID:   fmt.Sprintf("res-%d", i),
				ResourceName: "resource",
				Node:         "node-1",
				Instance:     "instance-1",
				Message:      "Test alert",
				Value:        float64(i),
				Threshold:    80,
				StartTime:    time.Now(),
			}
			manager.SendAlert(alert)
		}
	}()

	wg.Wait()

	time.Sleep(500 * time.Millisecond)
}
