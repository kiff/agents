package attach

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// A Word or Excel file is a zip of XML, from someone else's computer: it is
// opened with limits before anything is read, and nothing in it runs.
const (
	maxEntries  = 1000    // files in the zip
	maxUnzipped = 64 * mb // all of them, uncompressed
	maxPart     = 16 * mb // one of them, uncompressed
	maxRatio    = 200     // uncompressed over compressed, for one part
)

// ErrUnsafe: a Word or Excel file that isn't opened.
var ErrUnsafe = errors.New("attach: the file can't be opened safely")

// office opens data as an Office zip: within the limits, and with no macros.
// It returns its parts by name.
func office(data []byte) (map[string]*zip.File, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: not a zip: %v", ErrUnsafe, err)
	}
	if len(zr.File) > maxEntries {
		return nil, fmt.Errorf("%w: %d parts", ErrUnsafe, len(zr.File))
	}
	parts := map[string]*zip.File{}
	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if f.UncompressedSize64 > maxPart || total > maxUnzipped ||
			(f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > maxRatio) ||
			(f.CompressedSize64 == 0 && f.UncompressedSize64 > 0) {
			return nil, fmt.Errorf("%w: it unpacks too large", ErrUnsafe)
		}
		name := strings.ToLower(f.Name)
		if strings.HasSuffix(name, "vbaproject.bin") || strings.HasSuffix(name, ".bin") && strings.Contains(name, "activex") {
			return nil, fmt.Errorf("%w: it has macros", ErrUnsafe)
		}
		parts[f.Name] = f
	}
	return parts, nil
}

// read reads one part, never more than its limit, whatever its header said.
func read(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxPart+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPart {
		return nil, fmt.Errorf("%w: it unpacks too large", ErrUnsafe)
	}
	return b, nil
}

// decoder reads XML strictly: a document type declaration (where entities
// are declared) refuses the part, so nothing is expanded or fetched.
func decoder(b []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	return d
}

// tokens walks a part's XML, refusing a DTD, calling fn for each token.
func tokens(b []byte, fn func(xml.Token) error) error {
	d := decoder(b)
	for {
		t, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnsafe, err)
		}
		if _, ok := t.(xml.Directive); ok {
			return fmt.Errorf("%w: it declares a document type", ErrUnsafe)
		}
		if err := fn(t); err != nil {
			return err
		}
	}
}

// limited is text that stops at MaxText.
type limited struct{ strings.Builder }

func (l *limited) add(s string) error {
	if l.Len()+len(s) > MaxText {
		return fmt.Errorf("its text is over %s", human(MaxText))
	}
	l.WriteString(s)
	return nil
}

// wordText is a .docx's body: each paragraph a line, each table row its cells
// joined by tabs.
func wordText(data []byte) ([]byte, error) {
	parts, err := office(data)
	if err != nil {
		return nil, err
	}
	doc, ok := parts["word/document.xml"]
	if !ok {
		return nil, fmt.Errorf("%w: no document in it", ErrUnsafe)
	}
	b, err := read(doc)
	if err != nil {
		return nil, err
	}
	var out limited
	var para strings.Builder
	inText, inCell := false, false
	err = tokens(b, func(t xml.Token) error {
		switch t := t.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				para.WriteByte('\t')
			case "br":
				para.WriteByte('\n')
			case "tc":
				inCell = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				sep := "\n"
				if inCell {
					sep = " "
				}
				if err := out.add(para.String() + sep); err != nil {
					return err
				}
				para.Reset()
			case "tc":
				inCell = false
				if err := out.add("\t"); err != nil {
					return err
				}
			case "tr":
				if err := out.add("\n"); err != nil {
					return err
				}
			}
		case xml.CharData:
			if inText {
				para.Write(t)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(out.String())), nil
}

// excelText is each sheet of an .xlsx as CSV, under its name.
func excelText(data []byte) ([]byte, error) {
	parts, err := office(data)
	if err != nil {
		return nil, err
	}
	var shared []string
	if f, ok := parts["xl/sharedStrings.xml"]; ok {
		b, err := read(f)
		if err != nil {
			return nil, err
		}
		if shared, err = sharedStrings(b); err != nil {
			return nil, err
		}
	}
	sheets, err := sheetNames(parts)
	if err != nil {
		return nil, err
	}
	var out limited
	for _, s := range sheets {
		f, ok := parts[s.part]
		if !ok {
			continue
		}
		b, err := read(f)
		if err != nil {
			return nil, err
		}
		rows, err := sheetRows(b, shared)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		if err := w.WriteAll(rows); err != nil {
			return nil, err
		}
		if err := out.add("Sheet: " + s.name + "\n" + buf.String() + "\n"); err != nil {
			return nil, err
		}
	}
	return []byte(strings.TrimSpace(out.String())), nil
}

// sharedStrings is the workbook's string table, in order.
func sharedStrings(b []byte) ([]string, error) {
	var out []string
	var cur strings.Builder
	inSI, inT := false, false
	total := 0
	err := tokens(b, func(t xml.Token) error {
		switch t := t.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = inSI
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "si":
				inSI = false
				out = append(out, cur.String())
			}
		case xml.CharData:
			if inT {
				total += len(t)
				if total > MaxText {
					return fmt.Errorf("its text is over %s", human(MaxText))
				}
				cur.Write(t)
			}
		}
		return nil
	})
	return out, err
}

type sheet struct{ name, part string }

// sheetNames is the workbook's sheets in order, each with its part, read from
// the workbook and its relationships.
func sheetNames(parts map[string]*zip.File) ([]sheet, error) {
	rels := map[string]string{}
	if f, ok := parts["xl/_rels/workbook.xml.rels"]; ok {
		b, err := read(f)
		if err != nil {
			return nil, err
		}
		err = tokens(b, func(t xml.Token) error {
			if s, ok := t.(xml.StartElement); ok && s.Name.Local == "Relationship" {
				var id, target string
				for _, a := range s.Attr {
					switch a.Name.Local {
					case "Id":
						id = a.Value
					case "Target":
						target = a.Value
					}
				}
				rels[id] = path.Join("xl", strings.TrimPrefix(target, "/xl/"))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	f, ok := parts["xl/workbook.xml"]
	if !ok {
		return nil, fmt.Errorf("%w: no workbook in it", ErrUnsafe)
	}
	b, err := read(f)
	if err != nil {
		return nil, err
	}
	var out []sheet
	err = tokens(b, func(t xml.Token) error {
		if s, ok := t.(xml.StartElement); ok && s.Name.Local == "sheet" {
			var name, rid string
			for _, a := range s.Attr {
				switch {
				case a.Name.Local == "name":
					name = a.Value
				case a.Name.Local == "id":
					rid = a.Value
				}
			}
			p := rels[rid]
			if p == "" {
				p = fmt.Sprintf("xl/worksheets/sheet%d.xml", len(out)+1)
			}
			out = append(out, sheet{name: name, part: p})
		}
		return nil
	})
	return out, err
}

// sheetRows is a sheet's cells as rows of text, each cell at its column.
func sheetRows(b []byte, shared []string) ([][]string, error) {
	var rows [][]string
	var row []string
	var ref, typ string
	var val strings.Builder
	inV, inIS := false, false
	total := 0
	err := tokens(b, func(t xml.Token) error {
		switch t := t.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = nil
			case "c":
				ref, typ = "", ""
				val.Reset()
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "r":
						ref = a.Value
					case "t":
						typ = a.Value
					}
				}
			case "v":
				inV = true
			case "is":
				inIS = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "is":
				inIS = false
			case "c":
				s := val.String()
				if typ == "s" {
					if i, err := strconv.Atoi(s); err == nil && i >= 0 && i < len(shared) {
						s = shared[i]
					}
				}
				col := column(ref)
				if col < len(row) || col > 16383 {
					col = len(row)
				}
				total += col - len(row) // each empty cell is a comma
				if total > MaxText {
					return fmt.Errorf("its text is over %s", human(MaxText))
				}
				for len(row) < col {
					row = append(row, "")
				}
				row = append(row, s)
				total += len(s)
				if total > MaxText {
					return fmt.Errorf("its text is over %s", human(MaxText))
				}
			case "row":
				rows = append(rows, row)
			}
		case xml.CharData:
			if inV || inIS {
				val.Write(t)
			}
		}
		return nil
	})
	return rows, err
}

// column is a cell reference's column, from 0 ("B3" is 1); -1 for none.
func column(ref string) int {
	n := 0
	i := 0
	for ; i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z'; i++ {
		n = n*26 + int(ref[i]-'A'+1)
	}
	if i == 0 {
		return -1
	}
	return n - 1
}
