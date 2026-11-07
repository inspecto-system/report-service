// Package render turns the data of an inspection into the HTML of its PDF report and the PDF
// through Gotenberg (ADR-012, ADR-026). Fonts and photos are embedded as data URIs, so rendering
// never reaches outside the HTML (THREAT_MODEL T-12).
package render

import (
	"bytes"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/core"
)

//go:embed report.html
var templates embed.FS

//go:embed fonts/*.ttf
var fontFiles embed.FS

var page = template.Must(template.ParseFS(templates, "report.html"))

// fontFaces declares the fonts of the report; the CSS is built from embedded files, never from input.
var fontFaces = mustFontFaces([]fontFace{
	{family: "PT Sans", weight: 400, file: "PT_Sans-Web-Regular.ttf"},
	{family: "PT Sans", weight: 700, file: "PT_Sans-Web-Bold.ttf"},
	{family: "PT Serif", weight: 400, file: "PT_Serif-Web-Regular.ttf"},
	{family: "PT Serif", weight: 700, file: "PT_Serif-Web-Bold.ttf"},
})

type fontFace struct {
	family string
	weight int
	file   string
}

// Document is what the template shows.
type Document struct {
	Locale     string
	L          Labels
	Data       core.ReportData
	Photos     map[uuid.UUID]template.URL
	Fonts      template.CSS
	Severities []string
	zone       *time.Location
}

// Date formats a timestamp in the time zone of the report.
func (d Document) Date(t time.Time) string {
	layout := "02.01.2006 15:04"
	if d.Locale == "en" {
		layout = "2006-01-02 15:04"
	}
	return t.In(d.zone).Format(layout)
}

// HTML renders the report; photos maps photo ids to JPEG bytes (missing ones are left out).
func HTML(data core.ReportData, photos map[uuid.UUID][]byte, locale string, zone *time.Location) ([]byte, error) {
	embedded := make(map[uuid.UUID]template.URL, len(photos))
	for id, jpeg := range photos {
		// The bytes come from media-service, which re-encodes every photo as JPEG (ADR-025).
		embedded[id] = template.URL("data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg)) //nolint:gosec // trusted JPEG, see above
	}
	doc := Document{
		Locale:     locale,
		L:          LabelsFor(locale),
		Data:       data,
		Photos:     embedded,
		Fonts:      fontFaces,
		Severities: severityOrder,
		zone:       zone,
	}
	var out bytes.Buffer
	if err := page.Execute(&out, doc); err != nil {
		return nil, fmt.Errorf("render report HTML: %w", err)
	}
	return out.Bytes(), nil
}

func mustFontFaces(faces []fontFace) template.CSS {
	var css strings.Builder
	for _, f := range faces {
		data, err := fontFiles.ReadFile("fonts/" + f.file)
		if err != nil {
			panic(fmt.Sprintf("embedded font %s: %v", f.file, err))
		}
		fmt.Fprintf(&css, "@font-face { font-family: %q; font-weight: %d; src: url(data:font/ttf;base64,%s) format(\"truetype\"); }\n",
			f.family, f.weight, base64.StdEncoding.EncodeToString(data))
	}
	return template.CSS(css.String()) //nolint:gosec // built from embedded font files only
}
