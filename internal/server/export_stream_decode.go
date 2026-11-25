package server

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/jsonscan"
	"github.com/PerpetualSoftware/pad/internal/models"
)

type exportStreamChecks struct {
	repair *nulRepairTally
	seen   map[string]string
}

func (c *exportStreamChecks) MemberName(name string) error {
	if strings.IndexByte(name, 0) >= 0 {
		if c.repair != nil && c.repair.Enabled {
			c.repair.Replaced++
		} else {
			return errJSONBodyNUL
		}
	}
	folded := jsonscan.FoldName([]byte(name))
	if first, exists := c.seen[folded]; exists {
		return &models.RepeatedMemberError{Member: first}
	}
	c.seen[folded] = name
	return nil
}

func (c *exportStreamChecks) Record(member string, raw []byte) ([]byte, error) {
	if encodedFieldKey([]byte(member)) {
		wrapped, err := wrapJSONMember(member, raw)
		if err != nil {
			return nil, err
		}
		wrapped = c.repair.Apply(wrapped)
		found := scanRequestBody(wrapped, foldNone)
		if found.nul {
			return nil, errJSONBodyNUL
		}
		if found.repeat != "" {
			return nil, &models.RepeatedMemberError{Member: found.repeat}
		}
		return raw, nil
	}

	raw = c.repair.Apply(raw)
	found := scanRequestBody(raw, foldNone)
	if found.nul {
		return nil, errJSONBodyNUL
	}
	if found.repeat != "" {
		return nil, &models.RepeatedMemberError{Member: found.repeat}
	}
	return raw, nil
}

func wrapJSONMember(name string, raw []byte) ([]byte, error) {
	keyBytes, err := json.Marshal(name)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Grow(len(keyBytes) + len(raw) + 3)
	buf.WriteByte('{')
	buf.Write(keyBytes)
	buf.WriteByte(':')
	buf.Write(raw)
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func decodeExportStream(r io.Reader, repair *nulRepairTally) (*models.WorkspaceExport, error) {
	checks := &exportStreamChecks{
		repair: repair,
		seen:   make(map[string]string, 16),
	}
	return models.DecodeWorkspaceExport(r, checks)
}

type readErrRecorder struct {
	r   io.Reader
	err error
}

func (e *readErrRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}
