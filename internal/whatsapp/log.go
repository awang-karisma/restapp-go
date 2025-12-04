package whatsapp

import (
	"fmt"
	"log/slog"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// slogWaLogger adapts slog to the whatsmeow Logger interface.
type slogWaLogger struct {
	l *slog.Logger
}

// newWaSlogLogger builds a whatsmeow Logger backed by slog.
func newWaSlogLogger(module string) waLog.Logger {
	return &slogWaLogger{l: slog.Default().With("module", module)}
}

func (s *slogWaLogger) Errorf(msg string, args ...interface{}) { s.l.Error(fmt.Sprintf(msg, args...)) }
func (s *slogWaLogger) Warnf(msg string, args ...interface{})  { s.l.Warn(fmt.Sprintf(msg, args...)) }
func (s *slogWaLogger) Infof(msg string, args ...interface{})  { s.l.Info(fmt.Sprintf(msg, args...)) }
func (s *slogWaLogger) Debugf(msg string, args ...interface{}) { s.l.Debug(fmt.Sprintf(msg, args...)) }
func (s *slogWaLogger) Sub(module string) waLog.Logger {
	return &slogWaLogger{l: s.l.With("module", module)}
}
