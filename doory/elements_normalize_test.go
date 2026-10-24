// PROBE-ANCHORED conformance for the elements normaliser: the pinned oracle's
// apps/elements/normalize.py produced these exact documents (probe
// 0017-elements-normalize: 12 inputs x 4 specs). Mutation: paint resolution,
// inline-style handling, id/whitespace tidying, sizing or the XML writer
// flips these.
package doory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type normalizeFixture struct {
	Input string `json:"input"`
	Spec  string `json:"spec"`
	OK    bool   `json:"ok"`
	SVG   string `json:"svg"`
	Error string `json:"error"`
}

// fixtureInputs mirrors the inputs of the probe generator (kept in the test so
// the fixture stays self-describing about what was fed in).
var normalizeInputs = map[string]string{
	"currentcolor":      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M4 4l16 16"/><circle cx="12" cy="12" r="3"/></svg>`,
	"named-and-rgb":     `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="rebeccapurple"/><rect x="1" y="1" width="4" height="4" fill="rgb(255, 0, 0)"/><rect x="5" y="5" width="4" height="4" fill="hsl(120, 100%, 50%)" opacity="0.5"/></svg>`,
	"inline-style":      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 8 8"><g style="fill:#00ff00;stroke:#000000"><path style="stroke-width: 3; fill: none !important" d="M1 1L7 7"/></g></svg>`,
	"hidden-helper":     `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 12 12"><path d="M0 0h12v12H0z" fill="none"/><path d="M2 2h8v8H2z" fill="#123456"/><g display="none"><path d="M0 0" fill="#f00"/></g></svg>`,
	"use-ref":           `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 24 24" fill="currentColor"><defs><path id="dot" d="M0 0h4v4H0z" fill="#ff0000"/></defs><use xlink:href="#dot" x="2" y="2"/><use href="#dot" x="12" y="12" style="fill:#0000ff"/></svg>`,
	"width-height-only": `<svg xmlns="http://www.w3.org/2000/svg" width="36" height="18"><rect width="36" height="18" fill="#abcdef"/></svg>`,
	"title-desc-ids":    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><title>Box</title><desc>A box</desc><g id="unused"><rect id="also-unused" width="16" height="16" fill="#222222"/></g><defs><linearGradient id="g1"><stop offset="0" stop-color="#111111"/><stop offset="1" stop-color="#eeeeee"/></linearGradient></defs><rect width="8" height="8" fill="url(#g1)"/></svg>`,
	"text-preserved":    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 40 20"><text x="2" y="16" fill="#333333">Hi <tspan fill="#ff0000">there</tspan> </text><g>  <rect width="2" height="2" fill="#00ff00"/>  </g></svg>`,
	"opacity-alpha":     `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 4 4"><rect width="4" height="4" fill="rgba(0, 0, 255, 0.25)"/><rect width="2" height="2" fill="#ff0000" fill-opacity="0.5"/></svg>`,
	"no-shapes":         `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 4 4"><title>empty</title></svg>`,
	"not-svg":           `<html xmlns="http://www.w3.org/1999/xhtml"><body/></html>`,
	"broken":            `<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0"</svg>`,
}

var normalizeSpecs = map[string]normalizeSpec{
	"mono24":      {Mode: "mono", Size: floatPtr(24)},
	"mono24frame": {Mode: "mono", Size: floatPtr(24), Frame: true},
	"colour-keep": {Mode: "colour"},
	"mono-custom": {Mode: "mono", Color: "#112233", Size: floatPtr(32)},
}

func floatPtr(v float64) *float64 { return &v }

func TestNormalizeConformance(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "test", "parity", "probes", "0017-elements-normalize",
		"fixtures.json"))
	if err != nil {
		t.Fatalf("fixtures: %v", err)
	}
	var fixtures []normalizeFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Input+"/"+fixture.Spec, func(t *testing.T) {
			input, ok := normalizeInputs[fixture.Input]
			if !ok {
				t.Fatalf("unknown fixture input %q", fixture.Input)
			}
			spec := normalizeSpecs[fixture.Spec]
			got, nerr := normalizeSVG([]byte(input), spec)
			if !fixture.OK {
				if nerr == nil {
					t.Fatalf("expected an error (%s), got %q", fixture.Error, string(got))
				}
				if nerr.Error() != fixture.Error {
					t.Fatalf("error %q, want %q", nerr.Error(), fixture.Error)
				}
				return
			}
			if nerr != nil {
				t.Fatalf("unexpected error: %v", nerr)
			}
			if string(got) != fixture.SVG {
				t.Fatalf("normalized SVG differs:\n got: %s\nwant: %s", string(got), fixture.SVG)
			}
		})
	}
}
