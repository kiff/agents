package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every language the apps speak is recognised in a message written in it (tafs app/strings/languages.json, with Hindi,
// Urdu and Turkmen), by its script when the script is its own and by its common words when the script is shared. The
// sentences are ordinary customer messages: a thanks, an order, a parcel on its way.
func TestEveryAppLanguageIsRecognised(t *testing.T) {
	for want, text := range map[string]string{
		// Latin script.
		"en": "Your refund has been issued. Thanks for your order.",
		"es": "Hola, hemos recibido su pedido y el reembolso está en camino. Gracias por su paciencia.",
		"pt": "Olá, recebemos o seu pedido e o reembolso está a caminho. Obrigado pela paciência.",
		"fr": "Bonjour, nous avons bien reçu votre commande et le remboursement est en route. Merci de votre patience.",
		"de": "Hallo, wir haben Ihre Bestellung erhalten und die Rückerstattung ist auf dem Weg. Danke für Ihre Geduld.",
		"it": "Ciao, abbiamo ricevuto il tuo ordine e il rimborso è in arrivo. Grazie per la pazienza.",
		"nl": "Uw bestelling is verzonden en wij hebben de factuur ook bijgevoegd. Graag gedaan.",
		"pl": "Dziękujemy za zamówienie. Twoje zamówienie jest w drodze i nie wymaga nic więcej.",
		"ca": "Gràcies per la seva comanda. Ens plau informar-vos que és en camí.",
		"et": "Tere! Teie tellimus on tehtud ja see saadetakse meie ladudest, palun oodake.",
		"lt": "Labas! Jūsų užsakymas yra išsiųstas ir buvo patvirtintas. Ačiū.",
		"lv": "Sveiki! Jūsu pasūtījums ir nosūtīts un tas ir apstiprināts. Paldies.",
		"cs": "Dobrý den, děkujeme za objednávku. Váš balík je na cestě a bude doručen brzy.",
		"sk": "Dobrý deň, ďakujeme za objednávku. Váš balík je na ceste a bol odoslaný.",
		"sl": "Zdravo, hvala za naročilo. Vaš paket je na poti in bil je poslan.",
		"hu": "Szia! Köszönöm a rendelést. A csomag a mi raktárunkból van úton, kérem várjon.",
		"ro": "Bună! Mulțumim pentru comanda dumneavoastră. Pachetul este în drum și vă rog să așteptați.",
		"hr": "Hvala na narudžbi. Molim, vaša narudžba je na putu i bio sam sretan da se javim.",
		"sr": "Hvala na porudžbini. Vaša porudžbina je na putu, šta još mogu da uradim ovde?",
		"tr": "Merhaba, siparişiniz için teşekkürler. Paketiniz yolda ve çok yakında teslim edilecek.",
		"id": "Halo, terima kasih atas pesanan Anda. Paket ini sudah dikirim dan tidak ada masalah.",
		"vi": "Chào bạn, cảm ơn bạn đã đặt hàng. Đơn hàng của bạn đã được gửi và không có vấn đề.",
		"sq": "Përshëndetje, faleminderit për porosinë. Paketa juaj është në rrugë dhe nuk ka probleme.",
		"az": "Salam, sifarişiniz üçün təşəkkür edirik. Bağlama göndərildi və problem yoxdur, biz buradayıq.",
		"tk": "Salam! Sargydyňyz üçin sagbol. Ammarlama ýolda we hiç hili problema ýok, biz bu ýerde.",
		// Cyrillic script, shared: told apart by words.
		"bg": "Здравейте, благодаря за поръчката. Пратката е на път и това е всичко, моля изчакайте.",
		"mk": "Здраво, благодарам за нарачката. Пратката е на пат и тоа е сè, ве молам почекајте.",
		"mn": "Сайн байна уу? Захиалга баярлалаа. Таны ачаа замдаа явж байна, энэ бол бүх зүйл.",
		// Arabic script, shared with Urdu.
		"ar": "مرحبا، شكرا لطلبك. هذا الطرد في الطريق ونحن هنا إن كان لديك ما تسأل عنه.",
		"ur": "ہیلو، آپ کے آرڈر کا شکریہ۔ آپ کا پارسل راستے میں ہے اور ہم یہاں ہیں۔",
		// Devanagari script, shared with Nepali.
		"ne": "नमस्ते, तपाईंको अर्डरको लागि धन्यवाद। तपाईं र हामी यहाँ छौँ, यो प्याकेज बाटोमा छ।",
		"hi": "नमस्ते, आपके ऑर्डर के लिए धन्यवाद। आपका पार्सल रास्ते में है और हम यहाँ हैं।",
		// A script that is one language's alone: the script says it.
		"el": "Γεια σας, ευχαριστούμε για την παραγγελία σας.",
		"he": "שלום, תודה על ההזמנה שלך.",
		"th": "สวัสดี ขอบคุณสำหรับคำสั่งซื้อของคุณ",
		"lo": "ສະບາຍດີ ຂອບໃຈສໍາລັບການສັ່ງຊື້ຂອງທ່ານ",
		"km": "សួស្តី អរគុណសម្រាប់ការបញ្ជាទិញរបស់អ្នក",
		"ka": "გამარჯობა, გმადლობთ შეკვეთისთვის.",
		"hy": "Բարեւ, շնորհակալություն պատվերի համար։",
		"bn": "নমস্কার, আপনার অর্ডারের জন্য ধন্যবাদ।",
		"ko": "안녕하세요, 주문해 주셔서 감사합니다.",
		"ja": "こんにちは、ご注文ありがとうございます。",
		"zh": "您好，感谢您的订购。您的包裹已经寄出。",
	} {
		if got := language(text); got != want {
			t.Errorf("%s: read as %q: %s", want, got, text)
		}
	}
	// Serbian in Cyrillic is "sr" as well, and Japanese written mostly in kanji is still Japanese (it has kana).
	if got := language("Здраво, хвала на поруџбини. Пошиљка је на путу и то је све, молим вас сачекајте."); got != "sr" {
		t.Errorf("Serbian in Cyrillic: %q", got)
	}
	if got := language("ご注文の商品は本日発送しました。"); got != "ja" {
		t.Errorf("Japanese with kanji: %q", got)
	}
}

// Urdu is not Arabic, though both are written in the Arabic script: each is read by its own common words, an Urdu text is
// never taken for Arabic, and a text with too few of either gets no language at all, the default line.
func TestUrduIsNotTakenForArabic(t *testing.T) {
	urdu := "ہیلو، آپ کے آرڈر کا شکریہ۔ آپ کا پارسل راستے میں ہے اور ہم یہاں ہیں۔"
	if got := language(urdu); got != "ur" {
		t.Fatalf("Urdu: %q", got)
	}
	// Words only the two languages share, or neither's common ones: no language, never "ar".
	for _, text := range []string{
		"پارسل راستے میں", // Urdu words, one of the common ones
		"ابھی",            // one word
		"طلب",             // one Arabic common word: too few to say
		"سلام دوست عزیز، خوش آمدید",        // Persian: not a language of the apps
		"آپ کا شکریہ، ہم نے آرڈر بھیج دیا", // Urdu, with Urdu common words (below the arabic ones)
	} {
		if got := language(text); got == "ar" {
			t.Errorf("taken for Arabic: %q", text)
		}
	}
	// A short Urdu text with fewer than two common words gets the default line; a line for Arabic never heads it.
	d := &Disclosure{Line: "Written by an AI agent for Acme.", In: []DisclosureIn{{Tool: "send_reply", Fields: []string{"body"}}}, Lines: map[string]string{
		"ar": "كُتب بواسطة وكيل ذكاء اصطناعي لشركة Acme.",
		"ur": "ایک اے آئی ایجنٹ نے Acme کے لیے لکھا۔",
	}}
	send := func(text string) string {
		in, _ := json.Marshal(map[string]string{"body": text})
		var out struct{ Body string }
		_ = json.Unmarshal([]byte(strings.ToLower(string(d.First("send_reply", in)))), &out)
		var raw map[string]string
		_ = json.Unmarshal(d.First("send_reply", in), &raw)
		return raw["body"]
	}
	if got := send(urdu); !strings.HasPrefix(got, d.Lines["ur"]) {
		t.Errorf("an Urdu message is headed by the Urdu line: %q", got)
	}
	if got := send("پارسل راستے میں"); strings.HasPrefix(got, d.Lines["ar"]) || !strings.HasPrefix(got, d.Line) {
		t.Errorf("a short Urdu text is headed by the default line: %q", got)
	}
	arabic := "مرحبا، شكرا لطلبك. هذا الطرد في الطريق ونحن هنا إن كان لديك ما تسأل عنه."
	if got := send(arabic); !strings.HasPrefix(got, d.Lines["ar"]) {
		t.Errorf("an Arabic message is headed by the Arabic line: %q", got)
	}
}

// A script several languages share says no language by itself, and two languages that share most of their common words
// tie: no language either, the default line, never a guess. Nepali and Hindi, Bulgarian, Macedonian and Serbian,
// Croatian and Serbian in Latin script.
func TestAScriptSeveralLanguagesShareSaysNoLanguageByItself(t *testing.T) {
	for _, text := range []string{
		"тест",   // Cyrillic, no common words
		"नमस्ते", // Devanagari, a word both have
		"नमस्ते धन्यवाद", // both words are in both lists: a tie
		"и е на",                         // words of all three Cyrillic languages
		"Hvala na narudžbi, molim vas.",  // Croatian, but with only words Serbian has too: ...
		"Hvala, molim, je na putu i bio", // ... a tie between Croatian and Serbian
	} {
		if got := language(text); got == "ne" || got == "hi" || got == "bg" || got == "mk" || got == "mn" || (got == "sr" && strings.Contains(text, "Hvala")) {
			t.Errorf("a guess between languages: %q → %q", text, got)
		}
	}
}

// A message is read by the script most of its letters are in: a name, a product or a few words in another script don't
// change it, and a text with no script in the majority, no letters, or too few words says no language.
func TestALanguageIsTheMajorityScriptsOrNone(t *testing.T) {
	for text, want := range map[string]string{
		"안녕하세요, 주문해 주셔서 감사합니다. Order 1043 hello":                                "ko",
		"Gracias, hemos enviado su pedido de 東京 hoy. Gracias por su paciencia.": "es",
		"": "", "12345 !!!": "", "OK": "",
		"你好你好 abcd":      "", // half and half: no majority
		"안녕 hello world": "", // Latin is most, but has no common words
		"<p lang=\"en\" title=\"the order is for you and your thanks\">Hola, gracias.</p>": "es", // the tag's English words are not words: the two Spanish ones are
	} {
		if got := language(text); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
	// Kana says Japanese; Han alone says Chinese; Hangul with some Han (Hanja) says Korean.
	// Words written with combining marks (a virama, a vowel sign) stay whole: Hindi and Nepali are told apart by them.
	for text, want := range map[string]string{"ऑर्डर कृपया": "hi", "अर्डर कृपया": "ne"} {
		if got := language(text); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
	for text, want := range map[string]string{"カタカナ": "ja", "ひらがな": "ja", "汉字": "zh", "한국어 漢字 한국어": "ko"} {
		if got := language(text); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
}

// A line is put first in the language of the message when the definition has it, for any of the languages the apps speak:
// a Hindi message reads its line in Hindi, a Turkmen one in Turkmen, a Korean one in Korean, and one in a language the
// definition has no line for reads the default.
func TestTheLineHeadsTheMessageInAnyLanguageTheDefinitionHas(t *testing.T) {
	lines := map[string]string{"hi": "एक एआई एजेंट ने Acme की ओर से लिखा।", "tk": "Acme adyndan AI agenti tarapyndan ýazyldy.", "ko": "Acme를 대신해 AI 에이전트가 작성했습니다.",
		"ja": "Acme のために AI エージェントが書きました。", "sr": "AI agent je napisao u ime Acme."}
	d := &Disclosure{Line: "Written by an AI agent on behalf of Acme.", Lines: lines, In: []DisclosureIn{{Tool: "send_reply", Fields: []string{"body"}}}}
	if err := d.Check(); err != nil {
		t.Fatal(err)
	}
	head := func(text string) string {
		in, _ := json.Marshal(map[string]string{"body": text})
		var raw map[string]string
		_ = json.Unmarshal(d.First("send_reply", in), &raw)
		return strings.SplitN(raw["body"], "\n\n", 2)[0]
	}
	for text, want := range map[string]string{
		"नमस्ते, आपके ऑर्डर के लिए धन्यवाद। आपका पार्सल रास्ते में है और हम यहाँ हैं।":           lines["hi"],
		"Salam! Sargydyňyz üçin sagbol. Ammarlama ýolda we hiç hili problema ýok, biz bu ýerde.": lines["tk"],
		"안녕하세요, 주문해 주셔서 감사합니다.":                                                                  lines["ko"],
		"こんにちは、ご注文ありがとうございます。":                                                                   lines["ja"],
		"Hvala na porudžbini. Vaša porudžbina je na putu, šta još mogu da uradim ovde?":          lines["sr"],
		"Здраво, хвала на поруџбини. Пошиљка је на путу и то је све, молим вас сачекајте.":       lines["sr"],
		"Merhaba, siparişiniz için teşekkürler. Paketiniz yolda ve çok yakında teslim edilecek.": d.Line, // Turkish: no line for it here
		"Your refund has been issued. Thanks for your order.":                                    d.Line,
	} {
		if got := head(text); got != want {
			t.Errorf("%q headed by %q, want %q", text, got, want)
		}
	}
}
