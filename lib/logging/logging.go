package logging

// Level describes the chosen log level.
type Level int

const (
	// NONE means no logging.
	NONE Level = iota
	// DEBUG turns on debug logs - its generally too much for production in normal
	// conditions but can help when developing and investigating problems in production.
	DEBUG
	// INFO is logs useful server information. This includes various information
	// about problems with client connections which is not Centrifugo errors but
	// in most situations malformed client behaviour.
	INFO
	// ERROR level logs only server errors. This is logging that means non-working
	// Centrifugo and maybe effort from developers/administrators to make things
	// work again.
	ERROR
)

// levelToString has matches between Level and its string representation.
var levelToString = map[Level]string{
	NONE:  "none",
	DEBUG: "debug",
	INFO:  "info",
	ERROR: "error",
}

// StringToLevel ...
var StringToLevel = map[string]Level{
	"none":  NONE,
	"debug": DEBUG,
	"info":  INFO,
	"error": ERROR,
}

// LevelString transforms Level to its string representation.
func LevelString(l Level) string {
	if t, ok := levelToString[l]; ok {
		return t
	}
	return ""
}

// Entry represents log entry.
type Entry struct {
	Level   Level
	Message string
	Fields  map[string]interface{}
}

// NewEntry helps to create Entry.
func NewEntry(level Level, message string, fields ...map[string]interface{}) Entry {
	var f map[string]interface{}
	if len(fields) > 0 {
		f = fields[0]
	}
	return Entry{
		Level:   level,
		Message: message,
		Fields:  f,
	}
}

// Logger can log entries.
type Logger interface {
	Log(entry Entry)
	Enabled(Level) bool
}

// Handler handles log entries in whatever way it wants.
type Handler func(Entry)

// New creates Logger instance with selected Level and Handler.
func New(level Level, handler Handler) *HandlerLogger {
	return &HandlerLogger{
		level:   level,
		handler: handler,
	}
}

// HandlerLogger calls provided Handler func when Entry received.
type HandlerLogger struct {
	level   Level
	handler Handler
}

// Log calls log handler with provided Entry.
func (l *HandlerLogger) Log(entry Entry) {
	if l == nil {
		return
	}
	if entry.Level >= l.level && l.handler != nil {
		l.handler(entry)
	}
}

// Enabled returns logging level used.
func (l *HandlerLogger) Enabled(level Level) bool {
	if l == nil {
		return false
	}
	return level >= l.level
}