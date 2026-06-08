package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultPipelineLogRetention = 72 * time.Hour

type FilePipelineLogger struct {
	Dir       string
	Retention time.Duration
	Now       func() time.Time

	mu sync.Mutex
}

func (l *FilePipelineLogger) LogPipelineEvent(ctx context.Context, event PipelineLogEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := strings.TrimSpace(l.Dir)
	if dir == "" {
		return nil
	}

	now := l.now()
	if event.Time.IsZero() {
		event.Time = now.UTC()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := l.cleanupLocked(dir, now); err != nil {
		return err
	}

	path := filepath.Join(dir, "pipeline-"+event.Time.UTC().Format("2006-01-02")+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func (l *FilePipelineLogger) cleanupLocked(dir string, now time.Time) error {
	retention := l.Retention
	if retention <= 0 {
		retention = defaultPipelineLogRetention
	}
	cutoff := now.UTC().Add(-retention)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "pipeline-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		dateText := strings.TrimSuffix(strings.TrimPrefix(name, "pipeline-"), ".jsonl")
		logDate, err := time.Parse("2006-01-02", dateText)
		if err != nil {
			continue
		}
		if logDate.Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove old pipeline log %s: %w", name, err)
			}
		}
	}
	return nil
}

func (l *FilePipelineLogger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}
