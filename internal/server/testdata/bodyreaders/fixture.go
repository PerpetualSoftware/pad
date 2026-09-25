// Package bodyreaders is the fixture for the type-checked body-reader census
// (BUG-2820). Each function reaches a request body through one of the forms a
// NAME-matching parser could not see. The census must find every one; the
// function names say which form, and the test asserts on them by name.
package bodyreaders

import (
	"context"
	"io"
	. "net/http"
	nh "net/http"
)

type holder struct{ req *nh.Request }

type embeds struct{ *nh.Request }

type aliasReq = nh.Request

type ctxKey struct{}

func StructField(h holder) { _, _ = io.ReadAll(h.req.Body) }

func ContextValue(ctx context.Context) {
	r := ctx.Value(ctxKey{}).(*nh.Request)
	_, _ = io.ReadAll(r.Body)
}

func TypeAlias(r *aliasReq) { _, _ = io.ReadAll(r.Body) }

func EmbeddedField(e embeds) { _, _ = io.ReadAll(e.Body) }

func EmbeddedMethod(e embeds) { _ = e.FormValue("x") }

func ImportAlias(r *nh.Request) { _ = r.ParseForm() }

func DotImport(r *Request) { _, _ = r.MultipartReader() }

func VarDecl(r *nh.Request) {
	var req = r
	_, _ = io.ReadAll(req.Body)
}

func CallDerived(ctx context.Context, r *nh.Request) {
	req := r.WithContext(ctx)
	_, _, _ = req.FormFile("f")
}

func NamedResult() (r *nh.Request) {
	_ = r.PostFormValue("x")
	return r
}

func RangeBinding(rs []*nh.Request) {
	for _, r := range rs {
		_ = r.PostForm
	}
}

func MethodValue(r *nh.Request) { f := r.ParseMultipartForm; _ = f(1) }

// The cases the retired parser's own controls (TestBodyReaderScanDiscriminates)
// held it to, kept so the census answers them too.

func DirectParam(w nh.ResponseWriter, r *nh.Request) { _, _ = io.ReadAll(r.Body) }

func ClosureParam() func(*nh.Request) {
	return func(hr *nh.Request) { _, _ = io.ReadAll(hr.Body) }
}

func LocalAlias(orig *nh.Request) {
	req := orig
	_, _ = io.ReadAll(req.Body)
}

// A VALUE copy still shares the Body: it is an interface holding the same
// reader.
func ValueCopy(r *nh.Request) {
	c := *r
	_, _ = io.ReadAll(c.Body)
}

func ClosureValueParam(r *nh.Request) {
	f := func(c nh.Request) { _, _ = io.ReadAll(c.Body) }
	f(*r)
}

func ReassignedWithContext(r *nh.Request) {
	r = r.WithContext(context.Background())
	_, _ = io.ReadAll(r.Body)
}

func MixedShortDecl(r *nh.Request) {
	r, ok := getReq()
	_ = ok
	_, _ = io.ReadAll(r.Body)
}

func getReq() (*nh.Request, bool) { return nil, false }

func InsideSwitch(r *nh.Request) {
	switch r.Method {
	case "POST":
		_, _ = io.ReadAll(r.Body)
	}
}

func AfterIfInitShadow(r *nh.Request) {
	if r := 1; r > 0 {
		_ = r
	}
	_, _ = io.ReadAll(r.Body)
}

// NOT readers. The census must count none of these: a same-named field on
// another type, a request method that does not touch the body, and a Body on a
// Response.
type payload struct{ Body []byte }

func NotARequest(p payload) int { return len(p.Body) }

func HeaderOnly(r *nh.Request) string { return r.Header.Get("x") }

func ResponseBody(resp *nh.Response) { _, _ = io.ReadAll(resp.Body) }

// The parser OVER-FLAGGED these two on purpose (it could not model a scope that
// rebinds the name). The type checker resolves them exactly: neither `r` is a
// request here.
type other struct{ Body string }

func ShadowedByClosureParam(r *nh.Request) int {
	f := func(r other) int { return len(r.Body) }
	return f(other{})
}

func ReboundInNestedBlock(r *nh.Request) int {
	n := 0
	{
		r := other{}
		n = len(r.Body)
	}
	return n
}

// This function does not read r.Body, it only talks about it.
func CommentOnly(r *nh.Request) int { return 1 }
