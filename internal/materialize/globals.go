package materialize

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/dop251/goja"
)

// installGlobals gives the runtime the host APIs the bundle expects and goja
// lacks. They are Go-native because the pure-JS fallbacks in the bundle's
// polyfills.ts were measurably slower under goja (the TASK-2198 spike), and
// they are installed BEFORE the bundle runs so the fallbacks see them and
// stand down. console is a sink: nothing the bundle logs may reach the
// worker's stdout, which is its protocol channel.
func installGlobals(vm *goja.Runtime) error {
	noop := func(goja.FunctionCall) goja.Value { return goja.Undefined() }
	console := vm.NewObject()
	for _, k := range []string{"log", "warn", "error", "info", "debug", "trace"} {
		if err := console.Set(k, noop); err != nil {
			return err
		}
	}
	if err := vm.Set("console", console); err != nil {
		return err
	}
	if err := vm.Set("atob", func(c goja.FunctionCall) goja.Value { return atob(vm, c) }); err != nil {
		return err
	}
	if err := vm.Set("btoa", func(c goja.FunctionCall) goja.Value { return btoa(vm, c) }); err != nil {
		return err
	}
	crypto := vm.NewObject()
	if err := crypto.Set("subtle", vm.NewObject()); err != nil {
		return err
	}
	if err := crypto.Set("getRandomValues", func(c goja.FunctionCall) goja.Value {
		arg := c.Argument(0)
		var b []byte
		if err := vm.ExportTo(arg, &b); err != nil {
			panic(vm.NewTypeError("getRandomValues: argument is not an integer typed array"))
		}
		if len(b) > 65536 {
			panic(vm.NewGoError(errQuota))
		}
		// b aliases the typed array's backing store, so this fills it.
		_, _ = rand.Read(b)
		return arg
	}); err != nil {
		return err
	}
	if err := vm.Set("crypto", crypto); err != nil {
		return err
	}
	if err := vm.Set("TextEncoder", func(call goja.ConstructorCall) *goja.Object { return newTextEncoder(vm, call) }); err != nil {
		return err
	}
	return vm.Set("TextDecoder", func(call goja.ConstructorCall) *goja.Object { return newTextDecoder(vm, call) })
}

type quotaError struct{}

func (quotaError) Error() string {
	return "QuotaExceededError: getRandomValues is limited to 65536 bytes"
}

var errQuota = quotaError{}

// atob per the HTML spec's forgiving-base64 decode: ASCII whitespace is
// removed, one or two trailing '=' are optional, and the result is a string of
// code units 0-255, one per byte.
func atob(vm *goja.Runtime, c goja.FunctionCall) goja.Value {
	s := strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\f', '\r', ' ':
			return -1
		}
		return r
	}, c.Argument(0).String())
	if len(s)%4 == 0 {
		s = strings.TrimSuffix(strings.TrimSuffix(s, "="), "=")
	}
	if len(s)%4 == 1 {
		panic(vm.NewTypeError("InvalidCharacterError: atob: invalid base64 length"))
	}
	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		panic(vm.NewTypeError("InvalidCharacterError: atob: " + err.Error()))
	}
	return vm.ToValue(latin1String(b))
}

// btoa: every code unit must be 0-255.
func btoa(vm *goja.Runtime, c goja.FunctionCall) goja.Value {
	units := utf16.Encode([]rune(c.Argument(0).String()))
	b := make([]byte, len(units))
	for i, u := range units {
		if u > 0xff {
			panic(vm.NewTypeError("InvalidCharacterError: btoa: character out of range"))
		}
		b[i] = byte(u)
	}
	return vm.ToValue(base64.StdEncoding.EncodeToString(b))
}

// latin1String maps each byte to the code point of the same value.
func latin1String(b []byte) string {
	ascii := true
	for _, x := range b {
		if x >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) + len(b)/2)
	for _, x := range b {
		sb.WriteRune(rune(x))
	}
	return sb.String()
}

// bytesOf reads a BufferSource: an ArrayBuffer, a typed array or a DataView.
func bytesOf(vm *goja.Runtime, v goja.Value) ([]byte, bool) {
	if ab, ok := v.Export().(goja.ArrayBuffer); ok {
		return ab.Bytes(), true
	}
	var b []byte
	if err := vm.ExportTo(v, &b); err != nil {
		return nil, false
	}
	return b, true
}

func newTextEncoder(vm *goja.Runtime, call goja.ConstructorCall) *goja.Object {
	u8 := vm.Get("Uint8Array")
	_ = call.This.Set("encoding", "utf-8")
	_ = call.This.Set("encode", func(c goja.FunctionCall) goja.Value {
		s := ""
		if a := c.Argument(0); !goja.IsUndefined(a) {
			s = a.String()
		}
		// goja's String() turns a lone surrogate into U+FFFD, which is what
		// the Encoding spec's encode does too.
		o, err := vm.New(u8, vm.ToValue(vm.NewArrayBuffer([]byte(s))))
		if err != nil {
			panic(err)
		}
		return o
	})
	_ = call.This.Set("encodeInto", func(c goja.FunctionCall) goja.Value {
		src := c.Argument(0).String()
		var dst []byte
		if err := vm.ExportTo(c.Argument(1), &dst); err != nil {
			panic(vm.NewTypeError("encodeInto: destination is not a Uint8Array"))
		}
		n, read := 0, 0
		for _, r := range src {
			l := utf8.RuneLen(r)
			if l < 0 {
				r, l = utf8.RuneError, 3
			}
			if n+l > len(dst) {
				break
			}
			utf8.EncodeRune(dst[n:], r)
			n += l
			if r >= 0x10000 {
				read += 2
			} else {
				read++
			}
		}
		res := vm.NewObject()
		_ = res.Set("read", read)
		_ = res.Set("written", n)
		return res
	})
	return nil
}

func newTextDecoder(vm *goja.Runtime, call goja.ConstructorCall) *goja.Object {
	if label := call.Argument(0); !goja.IsUndefined(label) {
		switch strings.ToLower(strings.TrimSpace(label.String())) {
		case "utf-8", "utf8", "unicode-1-1-utf-8":
		default:
			panic(vm.NewTypeError("TextDecoder: only utf-8 is supported"))
		}
	}
	fatal, ignoreBOM := false, false
	if o := call.Argument(1); !goja.IsUndefined(o) && !goja.IsNull(o) {
		obj := o.ToObject(vm)
		if v := obj.Get("fatal"); v != nil {
			fatal = v.ToBoolean()
		}
		if v := obj.Get("ignoreBOM"); v != nil {
			ignoreBOM = v.ToBoolean()
		}
	}
	_ = call.This.Set("encoding", "utf-8")
	_ = call.This.Set("fatal", fatal)
	_ = call.This.Set("ignoreBOM", ignoreBOM)
	_ = call.This.Set("decode", func(c goja.FunctionCall) goja.Value {
		a := c.Argument(0)
		if goja.IsUndefined(a) {
			return vm.ToValue("")
		}
		b, ok := bytesOf(vm, a)
		if !ok {
			panic(vm.NewTypeError("TextDecoder.decode: argument is not a BufferSource"))
		}
		if !ignoreBOM && len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
			b = b[3:]
		}
		if !utf8.Valid(b) {
			if fatal {
				panic(vm.NewTypeError("The encoded data was not valid for encoding utf-8"))
			}
			b = []byte(strings.ToValidUTF8(string(b), "�"))
		}
		return vm.ToValue(string(b))
	})
	return nil
}
