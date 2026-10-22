// Deterministic multipart bodies: ONE byte sequence built per step and sent
// identically to both sides (a fixed boundary keeps the bytes reproducible;
// responses — not requests — are what we compare).
package runner

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const multipartBoundary = "dooryparityboundary9f3c2a7b"

func buildMultipart(parts []MultipartPart, repoRoot string) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	for _, p := range parts {
		fmt.Fprintf(&buf, "--%s\r\n", multipartBoundary)
		if p.File != "" {
			path := p.File
			if !filepath.IsAbs(path) {
				path = filepath.Join(repoRoot, path)
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, "", rerr
			}
			fmt.Fprintf(&buf, "Content-Disposition: form-data; name=%q; filename=%q\r\n",
				p.Name, filepath.Base(path))
			fmt.Fprintf(&buf, "Content-Type: application/octet-stream\r\n\r\n")
			buf.Write(data)
			buf.WriteString("\r\n")
			continue
		}
		fmt.Fprintf(&buf, "Content-Disposition: form-data; name=%q\r\n\r\n", p.Name)
		buf.WriteString(p.Value)
		buf.WriteString("\r\n")
	}
	fmt.Fprintf(&buf, "--%s--\r\n", multipartBoundary)
	return buf.Bytes(), "multipart/form-data; boundary=" + multipartBoundary, nil
}

var _ = io.EOF
