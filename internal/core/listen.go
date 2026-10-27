package core

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Listen: the awake mode. Stream sources run as child processes; their
// lines pass a cheap filter (no model) and become signals; a tick runs a
// little after the first one (debounced) and on every interval. Between, the
// process sleeps on reads. wake (may be nil) is another way in (serve).
func Listen(ctx context.Context, cfg *Config, wake chan struct{}, tick func(context.Context), log io.Writer) {
	if wake == nil {
		wake = make(chan struct{}, 1)
	}
	chat := make(chan struct{}, 1) // a person waits for the answer: a short debounce
	var wg sync.WaitGroup
	for i := range cfg.Signals {
		src := &cfg.Signals[i]
		if src.Kind != "stream" {
			continue
		}
		w := wake
		if src.Chat {
			w = chat
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream(ctx, src, w, log)
		}()
	}
	tick(ctx)
	every := time.NewTicker(cfg.Every())
	defer every.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-every.C:
		case <-wake:
			select { // debounce: one tick for a burst
			case <-time.After(debounce):
			case <-ctx.Done():
				continue
			}
		case <-chat:
			select { // a line or two typed together: one tick
			case <-time.After(chatDebounce):
			case <-ctx.Done():
				continue
			}
		}
		tick(ctx)
	}
}

var debounce, chatDebounce = 20 * time.Second, 2 * time.Second

// streamFilter: lines worth a tick — an order, an advisory, news- or error-like text.
var streamFilter = regexp.MustCompile(`(?i)` + reIncident.String()[4:] + `|` + orderRe.String()[4:] + `|` + reAdvisory.String() + `|` + reUpdate.String()[4:])

// keep: whether a stream line becomes a signal (a chat stream: any line).
func keep(src *SignalSource, line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	return src.Chat || streamFilter.MatchString(line)
}

func stream(ctx context.Context, src *SignalSource, wake chan<- struct{}, log io.Writer) {
	for backoff := time.Second; ctx.Err() == nil; backoff = min(backoff*2, 5*time.Minute) {
		cmd := exec.CommandContext(ctx, src.Argv[0], src.Argv[1:]...)
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			fmt.Fprintf(log, "warden listen: %s: %v\n", src.Name, err)
		} else {
			started := time.Now()
			sc := bufio.NewScanner(out)
			sc.Buffer(nil, 1<<20)
			window, n, dropped := time.Now(), 0, 0
			for sc.Scan() {
				line := sc.Text()
				if !keep(src, line) {
					continue
				}
				if time.Since(window) > time.Minute {
					if dropped > 0 {
						if err := Drop(src.Dir, Signal{From: src.Name, Text: fmt.Sprintf("warden listen: %d more lines from %s were dropped (rate limit)", dropped, src.Name), Verified: true}); err != nil {
							fmt.Fprintf(log, "warden listen: %s: %v\n", src.Name, err)
						}
					}
					window, n, dropped = time.Now(), 0, 0
				}
				if n++; n > 120 {
					dropped++
					continue
				}
				if err := Drop(src.Dir, Signal{From: src.Name, Text: line, Verified: true}); err != nil {
					fmt.Fprintf(log, "warden listen: %s: %v\n", src.Name, err)
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			}
			_ = cmd.Wait() // the stream is restarted either way
			if time.Since(started) > time.Minute {
				backoff = time.Second
			}
			fmt.Fprintf(log, "warden listen: %s exited; restarting in %s\n", src.Name, backoff)
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
	}
}
