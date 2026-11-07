package render

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"

	"github.com/inspecto-system/report-service/internal/testsupport"
)

// cyrillicMapping matches a ToUnicode entry of a Cyrillic letter (U+0400…U+04FF).
var cyrillicMapping = regexp.MustCompile(`<04[0-9A-Fa-f]{2}>`)

// TestPDFHasPagesFontsAndCyrillic renders a real report with Gotenberg and checks the PDF with a
// full parser (ADR-026): page count, embedded PT fonts, Cyrillic text, size limit.
func TestPDFHasPagesFontsAndCyrillic(t *testing.T) {
	gotenberg := NewGotenberg(testsupport.StartGotenberg(t))
	photo := uuid.New()
	html, err := HTML(sample(photo), map[uuid.UUID][]byte{photo: jpegBytes(t)}, "ru", moscow)
	testsupport.Require(t, err == nil, "HTML: %v", err)

	const limit = 20 << 20
	pdf, err := gotenberg.PDF(context.Background(), html, limit)
	testsupport.Require(t, err == nil, "PDF: %v", err)
	testsupport.Require(t, bytes.HasPrefix(pdf, []byte("%PDF-")) && len(pdf) <= limit, "not a PDF within the limit: %d bytes", len(pdf))

	ctx, err := api.ReadAndValidate(context.Background(), bytes.NewReader(pdf), model.NewDefaultConfiguration())
	testsupport.Require(t, err == nil, "pdfcpu: %v", err)
	t.Logf("PDF: %d pages, %d bytes", ctx.PageCount, len(pdf))
	testsupport.Expect(t, ctx.PageCount >= 2, "pages = %d, want a multi-page report", ctx.PageCount)
	testsupport.Expect(t, PageCount(pdf) == ctx.PageCount, "PageCount = %d, pdfcpu counts %d", PageCount(pdf), ctx.PageCount)

	fonts, cyrillic := inspectFonts(ctx.XRefTable)
	t.Logf("embedded fonts: %v, cyrillic: %v", fonts, cyrillic)
	testsupport.Expect(t, fonts["PTSans"] && fonts["PTSerif"], "PT fonts are not embedded: %v", fonts)
	testsupport.Expect(t, cyrillic, "no Cyrillic text in the PDF")
	testsupport.Expect(t, gotenberg.Ping(context.Background()) == nil, "Ping failed")
}

// inspectFonts returns the families with an embedded font program and whether any font maps
// glyphs to Cyrillic letters.
func inspectFonts(xref *model.XRefTable) (map[string]bool, bool) {
	fonts := make(map[string]bool)
	cyrillic := false
	for nr, entry := range xref.Table {
		if entry == nil || entry.Free {
			continue
		}
		obj, err := xref.Dereference(*types.NewIndirectRef(nr, 0))
		dict, ok := obj.(types.Dict)
		if err != nil || !ok {
			continue
		}
		if family, embedded := embeddedFamily(dict); embedded {
			fonts[family] = true
		}
		cyrillic = cyrillic || mapsCyrillic(xref, dict)
	}
	return fonts, cyrillic
}

// embeddedFamily reads a font descriptor with an embedded font program.
func embeddedFamily(dict types.Dict) (string, bool) {
	name := dict.NameEntry("FontName")
	_, truetype := dict.Find("FontFile2")
	_, cff := dict.Find("FontFile3")
	if name == nil || (!truetype && !cff) {
		return "", false
	}
	return family(*name), true
}

// mapsCyrillic tells whether the ToUnicode map of a font has a Cyrillic letter.
func mapsCyrillic(xref *model.XRefTable, dict types.Dict) bool {
	ref, found := dict.Find("ToUnicode")
	if !found {
		return false
	}
	stream, _, err := xref.DereferenceStreamDict(ref)
	if err != nil || stream == nil || stream.DecodeWithLimit(1<<20) != nil {
		return false
	}
	return cyrillicMapping.Match(stream.Content)
}

// family turns a font name such as ABCDEF+PTSans-Regular (an embedded subset) into PTSans.
func family(name string) string {
	if _, rest, subset := strings.Cut(name, "+"); subset {
		name = rest
	}
	base, _, _ := strings.Cut(name, "-")
	return base
}
