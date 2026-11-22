package agent

import (
	"regexp"
	"strings"
	"unicode"
)

// Which language a message is written in, so the disclosure line heads it in that language (Disclosure.lineFor). It
// recognises every language the apps speak (tafs app/strings/languages.json, with Hindi, Urdu and Turkmen coming): a
// script that only one of them uses says it by itself (Hangul is Korean, kana is Japanese, Thai is Thai); a script that
// several share (Latin, Cyrillic, Arabic, Devanagari) is told apart by each language's common words. It only picks which
// line heads the text, so a text it is not sure of reads "": the default line, never a guess between two languages.

// markup is an html tag, which language reads past.
var markup = regexp.MustCompile(`<[^<>]*>`)

// commonWords are frequent short words of each language whose line a disclosure may have, per script group, enough to
// tell which one a message is written in. A word two languages share counts for both, so a text needs more of one's.
var commonWords = map[string]map[string]bool{
	// Latin script.
	"en": wordSet("the and is are you your to of for with we our this that it was has have been will be not on at by from thanks thank order please"),
	"es": wordSet("el la los las del de que y en por para con una un su sus tu tus te le es está están hemos ha han gracias pedido hola usted ya pero muy más"),
	"pt": wordSet("o os as do da dos das de que e em por para com uma um seu sua você não é está foi obrigado obrigada pedido olá já mas muito mais"),
	"fr": wordSet("le la les des du de et est pour pas vous nous une un que dans avec sur votre vos merci commande été sont bonjour déjà mais très plus"),
	"de": wordSet("der die das und ist nicht sie ihr ihre ein eine mit für auf den dem des zu wir danke bestellung wurde bitte hallo schon aber sehr mehr"),
	"it": wordSet("il lo gli della del di che e è per non una un con sono grazie ordine suo sua tuo tua ciao già ma molto più stato"),
	"nl": wordSet("de het een en van in is dat op te voor met zijn niet aan er maar ook om als bij uw u wij we hebben wordt bedankt bestelling graag hallo"),
	"pl": wordSet("i w nie na to się z że do jest są dla jak czy ale po od przez nasze twoje dziękujemy zamówienie proszę witaj"),
	"ca": wordSet("el la els les de del que i en per amb una un és són no més com gràcies comanda hola si ens vostè vostre seva"),
	"et": wordSet("ja on ei see et oma kui mis aga ka või meie teie tänan tellimus palun tere olen oled"),
	"lt": wordSet("ir yra ne kad tai su į iš kaip bet arba mūsų jūsų ačiū užsakymas prašau labas buvo"),
	"lv": wordSet("un ir nav ka tas ar uz no kā bet vai mūsu jūsu paldies pasūtījums lūdzu sveiki bija"),
	"cs": wordSet("a je se v na to že s z do pro ale nebo jako náš váš děkujeme objednávka prosím dobrý byl"),
	"sk": wordSet("a je sa v na to že s z do pre ale alebo ako náš váš ďakujeme objednávka prosím dobrý bol"),
	"sl": wordSet("in je se v na to da z iz do za ali kot naš vaš hvala naročilo prosim zdravo bil"),
	"hu": wordSet("a az és nem hogy is egy van meg de vagy mint mi ön önök köszönöm rendelés kérem szia volt"),
	"ro": wordSet("și în de la cu nu că pe un o este sunt pentru dar sau ca noi dumneavoastră vă mulțumim comandă rog bună"),
	"hr": wordSet("i je u na da se za s ali ili kao naš vaš hvala narudžba molim bok bio"),
	"sr": wordSet("i je u na da se za sa ali ili kao naš vaš hvala porudžbina molim zdravo bio šta ovde"),
	"tr": wordSet("ve bir bu için ile de da çok ama veya gibi biz siz teşekkürler sipariş lütfen merhaba oldu değil"),
	"id": wordSet("dan yang di ke dari untuk dengan ini itu tidak ada kami anda terima kasih pesanan silakan halo adalah"),
	"vi": wordSet("và của là có không cho một những các được trong tôi bạn chúng cảm ơn đơn hàng xin vui lòng chào"),
	"sq": wordSet("dhe në të për me një është nuk që por ose si ne ju faleminderit porosia lutem përshëndetje ishte"),
	"az": wordSet("və bir bu üçün ilə də çox amma kimi biz siz təşəkkür sifariş zəhmət olmasa salam idi deyil"),
	"tk": wordSet("we bir bu üçin bilen hem ýok emma ýaly biz siz sagbol sargyt haýyş salam boldy däl"),
	// Cyrillic script (Bulgarian, Macedonian, Serbian, Mongolian).
	"bg":      wordSet("и е на в за да се не с от това като но или ние вие благодаря поръчка моля здравейте беше"),
	"mk":      wordSet("и е на во за да се не со од тоа како но или ние вие благодарам нарачка ве молам здраво беше"),
	"sr-cyrl": wordSet("и је на у за да се не са од то као али или ми ви хвала поруџбина молим здраво био"),
	"mn":      wordSet("ба нь юм энэ байна уу үү гэж тэр бид та баярлалаа захиалга сайн байсан"),
	// Arabic script (Arabic, Urdu).
	"ar": wordSet("في من على أن إلى هذا هذه التي الذي مع ما لا هل نحن أنت شكرا طلب مرحبا كان"),
	"ur": wordSet("کی کے میں ہے ہیں سے اور کو یہ وہ نہیں پر کہ آپ ہم شکریہ آرڈر ہیلو تھا"),
	// Devanagari script (Nepali, Hindi).
	"ne": wordSet("र हो छ यो त्यो को का मा लाई हामी तपाईं धन्यवाद अर्डर कृपया नमस्ते थियो"),
	"hi": wordSet("और है का की के में से को यह वह नहीं पर कि आप हम धन्यवाद ऑर्डर कृपया नमस्ते था"),
}

// scriptLanguages is, for each script, the languages written in it that a disclosure may have a line for, in the order a
// tie is decided (it never is: a tie is no language). The key is also the commonWords key, but for Serbian, whose Latin
// and Cyrillic words are two lists with one line ("sr").
var scriptLanguages = []struct {
	table *unicode.RangeTable
	name  string
	// words are the commonWords keys read for it, and one is the language it says by itself when there is one.
	words []string
	only  string
}{
	{unicode.Latin, "latin", []string{"en", "es", "pt", "fr", "de", "it", "nl", "pl", "ca", "et", "lt", "lv", "cs", "sk", "sl", "hu", "ro", "hr", "sr", "tr", "id", "vi", "sq", "az", "tk"}, ""},
	{unicode.Cyrillic, "cyrillic", []string{"bg", "mk", "sr-cyrl", "mn"}, ""},
	{unicode.Arabic, "arabic", []string{"ar", "ur"}, ""},
	{unicode.Devanagari, "devanagari", []string{"ne", "hi"}, ""},
	{unicode.Greek, "greek", nil, "el"},
	{unicode.Hebrew, "hebrew", nil, "he"},
	{unicode.Thai, "thai", nil, "th"},
	{unicode.Lao, "lao", nil, "lo"},
	{unicode.Khmer, "khmer", nil, "km"},
	{unicode.Georgian, "georgian", nil, "ka"},
	{unicode.Armenian, "armenian", nil, "hy"},
	{unicode.Bengali, "bengali", nil, "bn"},
	{unicode.Hangul, "hangul", nil, "ko"},
	{unicode.Hiragana, "kana", nil, "ja"},
	{unicode.Katakana, "kana", nil, "ja"},
	{unicode.Han, "han", nil, "zh"},
}

func wordSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(words) {
		set[w] = true
	}
	return set
}

// language is the language text is written in, by the two-letter code a disclosure's Lines are keyed by ("zh" for
// Traditional Chinese, "sr" for Serbian in either script, "pt" for Brazilian Portuguese too), when the text says so
// clearly: its letters are mostly one script (more than half; a name in another script doesn't change it), and that
// script is one language's alone, or at least two of that language's common words are in it and more than any other
// language's of the same script. Empty otherwise: a short, mixed or unsure text gets the default line.
func language(text string) string {
	text = markup.ReplaceAllString(text, " ") // an html part's tags aren't its words
	letters := map[string]int{}
	total := 0
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		total++
		for _, s := range scriptLanguages {
			if unicode.Is(s.table, r) {
				letters[s.name]++
				break
			}
		}
	}
	if total == 0 {
		return ""
	}
	// Japanese is kana with the Han characters it is written with: kana says it, and Han then counts for it.
	if letters["kana"] > 0 {
		letters["kana"] += letters["han"]
		letters["han"] = 0
	}
	best, top := "", 0
	for name, n := range letters {
		if n > top || n == top && name < best {
			best, top = name, n
		}
	}
	if top*2 <= total { // not more than half of the letters: a mix
		return ""
	}
	for _, s := range scriptLanguages {
		if s.name != best {
			continue
		}
		if s.only != "" {
			return s.only
		}
		return byWords(text, s.words)
	}
	return ""
}

// byWords is the language among the commonWords keys given whose common words are most in text: at least two, and more
// than any other's. A Serbian list (Latin or Cyrillic) is the language "sr".
func byWords(text string, keys []string) string {
	counts := map[string]int{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsMark(r) }) {
		for _, k := range keys {
			if commonWords[k][w] {
				counts[k]++
			}
		}
	}
	best, top, second := "", 0, 0
	for _, k := range keys { // in the order given, so the same text always reads the same
		switch n := counts[k]; {
		case n > top:
			best, top, second = k, n, top
		case n > second:
			second = n
		}
	}
	if top < 2 || top == second {
		return ""
	}
	if best == "sr-cyrl" {
		return "sr"
	}
	return best
}
