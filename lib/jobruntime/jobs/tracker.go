package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobstore"
)

const progressBaseAttrs = 2

type stepRuntime struct {
	store            jobstore.Store
	logger           *slog.Logger
	runID            string
	jobID            string
	jobKind          string
	step             string
	retryGroupID     *string
	progressReporter ProgressReporter
}

type stepRuntimeParams struct {
	store            jobstore.Store
	logger           *slog.Logger
	runID            string
	jobID            string
	jobKind          string
	step             string
	retryGroupID     *string
	progressReporter ProgressReporter
}

type ProgressReport struct {
	RunID   string
	JobID   string
	JobKind string
	Step    string
	Done    int
	Total   int
	At      time.Time
}

// ProgressReporter is an optional runtime-only sink for in-flight progress.
type ProgressReporter interface {
	ReportProgress(ctx context.Context, report ProgressReport)
}

func newStepRuntime(p *stepRuntimeParams) StepRuntime {
	base := p.logger
	if base == nil {
		base = slog.Default()
	}

	return &stepRuntime{
		store:            p.store,
		logger:           base.With("run_id", p.runID, "job_id", p.jobID, "job_kind", p.jobKind, "step", p.step),
		runID:            p.runID,
		jobID:            p.jobID,
		jobKind:          p.jobKind,
		step:             p.step,
		retryGroupID:     p.retryGroupID,
		progressReporter: p.progressReporter,
	}
}

func (s *stepRuntime) RunID() string         { return s.runID }
func (s *stepRuntime) JobID() string         { return s.jobID }
func (s *stepRuntime) JobKind() string       { return s.jobKind }
func (s *stepRuntime) StepName() string      { return s.step }
func (s *stepRuntime) RetryGroupID() *string { return s.retryGroupID }
func (s *stepRuntime) Logger() *slog.Logger  { return s.logger }

func (s *stepRuntime) Debug(ctx context.Context, kind string, attrs ...slog.Attr) error {
	return s.record(ctx, EventLevelDebug, kind, attrs...)
}

func (s *stepRuntime) Debugf(ctx context.Context, kind, format string, args ...any) error {
	return s.Debug(ctx, kind, slog.String("message", fmt.Sprintf(format, args...)))
}

func (s *stepRuntime) Info(ctx context.Context, kind string, attrs ...slog.Attr) error {
	return s.record(ctx, EventLevelInfo, kind, attrs...)
}

func (s *stepRuntime) Infof(ctx context.Context, kind, format string, args ...any) error {
	return s.Info(ctx, kind, slog.String("message", fmt.Sprintf(format, args...)))
}

func (s *stepRuntime) Warn(ctx context.Context, kind string, attrs ...slog.Attr) error {
	return s.record(ctx, EventLevelWarn, kind, attrs...)
}

func (s *stepRuntime) Warnf(ctx context.Context, kind, format string, args ...any) error {
	return s.Warn(ctx, kind, slog.String("message", fmt.Sprintf(format, args...)))
}

func (s *stepRuntime) Error(ctx context.Context, kind string, attrs ...slog.Attr) error {
	return s.record(ctx, EventLevelError, kind, attrs...)
}

func (s *stepRuntime) Errorf(ctx context.Context, kind, format string, args ...any) error {
	return s.Error(ctx, kind, slog.String("message", fmt.Sprintf(format, args...)))
}

func (s *stepRuntime) Progress(ctx context.Context, done, total int, attrs ...slog.Attr) error {
	progressAttrs := make([]slog.Attr, 0, len(attrs)+progressBaseAttrs)
	progressAttrs = append(progressAttrs, slog.Int("done", done), slog.Int("total", total))
	progressAttrs = append(progressAttrs, attrs...)
	s.logger.LogAttrs(ctx, toSlogLevel(EventLevelInfo), "STEP_PROGRESS", progressAttrs...)
	if s.progressReporter != nil {
		s.progressReporter.ReportProgress(ctx, ProgressReport{
			RunID:   s.runID,
			JobID:   s.jobID,
			JobKind: s.jobKind,
			Step:    s.step,
			Done:    done,
			Total:   total,
			At:      time.Now().UTC(),
		})
	}
	return nil
}

func (s *stepRuntime) record(ctx context.Context, level EventLevel, kind string, attrs ...slog.Attr) error {
	meta := attrsToMeta(attrs)
	s.logger.LogAttrs(ctx, toSlogLevel(level), kind, attrs...)

	return s.store.RecordEvent(ctx, &jobstore.EventRecord{
		RunID:   s.runID,
		Level:   string(level),
		Kind:    kind,
		Meta:    meta,
		Step:    s.step,
		JobID:   s.jobID,
		JobKind: s.jobKind,
	})
}

func toSlogLevel(level EventLevel) slog.Level {
	switch level {
	case EventLevelDebug:
		return slog.LevelDebug
	case EventLevelWarn:
		return slog.LevelWarn
	case EventLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func attrsToMeta(attrs []slog.Attr) map[string]any {
	meta := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		resolved := attr.Value.Resolve()
		meta[attr.Key] = slogValueToAny(resolved)
	}
	return meta
}

func slogValueToAny(v slog.Value) any {
	switch v.Kind() {
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindString:
		return v.String()
	case slog.KindTime:
		return v.Time()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindGroup:
		group := v.Group()
		out := make(map[string]any, len(group))
		for _, attr := range group {
			out[attr.Key] = slogValueToAny(attr.Value.Resolve())
		}
		return out
	case slog.KindAny:
		return v.Any()
	default:
		return v.Any()
	}
}
