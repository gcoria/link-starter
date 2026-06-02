package linkoerr

import (
	"errors"
	"log/slog"
)

// errWithAttrs wraps an error with additional structured logging attributes.
type errWithAttrs struct {
	error
	attrs []slog.Attr
}

// WithAttrs wraps an error with additional structured logging attributes.
// args[i] is treated as a key if it is a string or an slog.Attr; otherwise,
// it is treated as a value with key "!BADKEY".
func WithAttrs(err error, args ...any) error {
	return &errWithAttrs{
		error: err,
		attrs: argsToAttr(args),
	}
}

// argsToAttr turns a list of typed or untyped values into a slice of slog.Attr.
// args[i] is treated as a key if it is a string or an slog.Attr; otherwise, it
// is treated as a value with key "!BADKEY".
func argsToAttr(args []any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(args))
	for i := 0; i < len(args); {
		switch key := args[i].(type) {
		case slog.Attr:
			attrs = append(attrs, key)
			i++
		case string:
			if i+1 >= len(args) {
				attrs = append(attrs, slog.String("!BADKEY", key))
				i++
			} else {
				attrs = append(attrs, slog.Any(key, args[i+1]))
				i += 2
			}
		default:
			attrs = append(attrs, slog.Any("!BADKEY", args[i]))
			i++
		}
	}
	return attrs
}

// Unwrap returns the wrapped error.
func (e *errWithAttrs) Unwrap() error {
	return e.error
}

// Attrs returns the structured logging attributes attached to this error.
func (e *errWithAttrs) Attrs() []slog.Attr {
	return e.attrs
}

// attrError is the interface for errors that carry structured logging attributes.
type attrError interface {
	Attrs() []slog.Attr
}

// Attrs recursively extracts all logging attributes from an error chain. In the
// case of duplicate keys, the outermost value takes precedence.
func Attrs(err error) []slog.Attr {
	var attrs []slog.Attr
	for err != nil {
		if ae, ok := err.(attrError); ok {
			attrs = append(attrs, ae.Attrs()...)
		}
		err = errors.Unwrap(err)
	}
	return attrs
}
