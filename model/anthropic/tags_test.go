package anthropic

import (
	"strings"
	"testing"
)

// prefix is the model format's tag prefix, built so no tool on the way writes
// it as a tag; open and closing make a bracketed tag of it.
var prefix = "ant" + "ml" + ":"

func open(name string) string    { return "<" + prefix + name + ">" }
func closing(name string) string { return "</" + prefix + name + ">" }

// A tag of the model's own format leaked into a reply's text never reaches the
// owner: "antml:final_answer_dialog Falta que apruebes…" did in Eval
// 42141761407 (the PM, qa-kiff). Whole, bracketed or bare, with emphasis or not;
// anything else is left as it is.
func TestALeakedTagIsStripped(t *testing.T) {
	for in, want := range map[string]string{
		"antml:final_answer_dialog Falta que apruebes un acceso…":                   "Falta que apruebes un acceso…",
		"**antml:final_answer_dialog** Falta que apruebes un acceso…":               "Falta que apruebes un acceso…",
		open("final_answer_dialog") + "Hola" + closing("final_answer_dialog"):       "Hola",
		"Listo.\n" + open("x a=\"b\"") + "Reembolsos está activo.":                  "Listo.\nReembolsos está activo.",
		"El mensaje antml no es una etiqueta, ni antml_x.":                          "El mensaje antml no es una etiqueta, ni antml_x.",
		"Una prueba: el modo de prueba, en una cuenta antigua <b>y</b> a las 9:00.": "Una prueba: el modo de prueba, en una cuenta antigua <b>y</b> a las 9:00.",
	} {
		if got := stripTags(in); got != want {
			t.Errorf("stripTags(%q) = %q, want %q", in, got, want)
		}
	}
}

// Streamed, a tag can come split across deltas: the filter holds back an end
// that could be a tag's start until the next delta settles it, and loses
// nothing that wasn't one.
func TestATagSplitAcrossDeltasIsStripped(t *testing.T) {
	for _, tc := range []struct {
		deltas []string
		want   string
	}{
		{[]string{"antml:final_", "answer_dialog Falta que ", "apruebes un acceso…"}, "Falta que apruebes un acceso…"},
		{[]string{"ant", "ml:fin", "al_answer_dialog Falta."}, "Falta."},
		{[]string{"**antml:final_answer", "_dialog** Hola"}, "Hola"},
		{[]string{"<" + prefix + "fi", "nal_answer_dialog>Hola", closing("final_answer_dialog")}, "Hola"},
		{[]string{"Es una", " prueba de a", "ntes y an", "tes."}, "Es una prueba de antes y antes."},
		{[]string{"Va a"}, "Va a"},
		{[]string{"x <", "b>y"}, "x <b>y"},
	} {
		var got strings.Builder
		f := &tagFilter{out: func(s string) { got.WriteString(s) }}
		for _, d := range tc.deltas {
			f.write(d)
		}
		f.flush()
		if got.String() != tc.want {
			t.Errorf("%q: got %q, want %q", tc.deltas, got.String(), tc.want)
		}
	}
}
