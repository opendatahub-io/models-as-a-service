package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Logger wraps zap.Logger to provide a structured logging interface
// aligned with KServe conventions.
type Logger struct {
	*zap.SugaredLogger

	level zapcore.Level
}

// Production returns a production-ready logger with INFO level.
// Use this for deployed environments.
func Production() *Logger {
	return New(false)
}

// Development returns a development logger with DEBUG level and colored output.
// Use this for local development and debugging.
func Development() *Logger {
	return New(true)
}

// New creates a new logger instance with KServe-compatible configuration.
// It supports different log levels (DEBUG, INFO, WARN, ERROR) and structured output.
// Prefer using Production() or Development() for better readability.
func New(debug bool) *Logger {
	var config zap.Config
	if debug {
		config = zap.NewDevelopmentConfig()
		config.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		config = zap.NewProductionConfig()
		config.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
		config.EncoderConfig.EncodeLevel = zapcore.LowercaseLevelEncoder
	}

	// KServe-style configuration
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	config.EncoderConfig.MessageKey = "message"
	config.EncoderConfig.LevelKey = "level"
	config.EncoderConfig.CallerKey = "caller"
	config.EncoderConfig.StacktraceKey = "stacktrace"
	config.OutputPaths = []string{"stdout"}
	config.ErrorOutputPaths = []string{"stderr"}

	// Build logger
	baseLogger, err := config.Build(
		zap.AddCaller(),
		zap.AddCallerSkip(1), // Skip this package in call stack
		zap.AddStacktrace(zapcore.ErrorLevel),
	)
	if err != nil {
		// Fallback to a basic logger if configuration fails
		baseLogger = zap.NewExample()
	}

	level := zapcore.InfoLevel
	if debug {
		level = zapcore.DebugLevel
	}

	return &Logger{
		SugaredLogger: baseLogger.Sugar(),
		level:         level,
	}
}

// NewFromEnv creates a logger based on environment variables.
// Checks DEBUG_MODE environment variable to determine log level.
func NewFromEnv() *Logger {
	debug := os.Getenv("DEBUG_MODE") == "true" || os.Getenv("DEBUG_MODE") == "1"
	return New(debug)
}

// WithFields returns a logger with additional structured fields.
// This follows KServe's pattern of using structured logging for better observability.
func (l *Logger) WithFields(fields ...any) *Logger {
	return &Logger{
		SugaredLogger: l.With(fields...),
		level:         l.level,
	}
}

// WithError returns a logger with an error field attached.
// This is a convenience method for error logging.
func (l *Logger) WithError(err error) *Logger {
	if err == nil {
		return l
	}
	return &Logger{
		SugaredLogger: l.With("error", err.Error()),
		level:         l.level,
	}
}

// WithRequestID returns a logger with a request_id field attached.
// This is the preferred method for request-scoped logging to enable
// correlation across logs without exposing sensitive tokens.
func (l *Logger) WithRequestID(requestID string) *Logger {
	if requestID == "" {
		return l
	}
	return &Logger{
		SugaredLogger: l.With("request_id", requestID),
		level:         l.level,
	}
}

// Debug logs a debug-level message with optional fields.
// Only logged when debug mode is enabled.
func (l *Logger) Debug(msg string, fields ...any) {
	if l.level <= zapcore.DebugLevel {
		l.Debugw(msg, fields...)
	}
}

// Info logs an info-level message with optional fields.
func (l *Logger) Info(msg string, fields ...any) {
	if l.level <= zapcore.InfoLevel {
		l.Infow(msg, fields...)
	}
}

// Warn logs a warning-level message with optional fields.
func (l *Logger) Warn(msg string, fields ...any) {
	if l.level <= zapcore.WarnLevel {
		l.Warnw(msg, fields...)
	}
}

// Error logs an error-level message with optional fields.
// Automatically includes stack trace in production mode.
func (l *Logger) Error(msg string, fields ...any) {
	if l.level <= zapcore.ErrorLevel {
		l.Errorw(msg, fields...)
	}
}

// Fatal logs a fatal-level message and exits the program.
func (l *Logger) Fatal(msg string, fields ...any) {
	l.Fatalw(msg, fields...)
}

// Sync flushes any buffered log entries.
// Should be called before program exit.
func (l *Logger) Sync() error {
	return l.SugaredLogger.Sync()
}
