package smtp

import (
	"strings"
	"testing"
)

func TestParseEmailPlainText(t *testing.T) {
	raw := "From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: Test\r\nContent-Type: text/plain\r\n\r\nHello, world!"
	email, err := ParseEmail(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseEmail() error: %v", err)
	}
	if email.From != "sender@example.com" {
		t.Errorf("From = %q, want sender@example.com", email.From)
	}
	if len(email.To) != 1 || email.To[0] != "recipient@example.com" {
		t.Errorf("To = %v, want [recipient@example.com]", email.To)
	}
	if email.Subject != "Test" {
		t.Errorf("Subject = %q, want Test", email.Subject)
	}
	if email.TextBody != "Hello, world!" {
		t.Errorf("TextBody = %q, want 'Hello, world!'", email.TextBody)
	}
}

func TestParseEmailHTML(t *testing.T) {
	raw := "From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: Test\r\nContent-Type: text/html\r\n\r\n<h1>Hello</h1>"
	email, err := ParseEmail(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseEmail() error: %v", err)
	}
	if email.HTMLBody != "<h1>Hello</h1>" {
		t.Errorf("HTMLBody = %q, want '<h1>Hello</h1>'", email.HTMLBody)
	}
}

func TestParseEmailMultipartAlternative(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Multi\r\n" +
		"Content-Type: multipart/alternative; boundary=boundary123\r\n" +
		"\r\n" +
		"--boundary123\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Plain text body\r\n" +
		"--boundary123\r\n" +
		"Content-Type: text/html\r\n" +
		"\r\n" +
		"<p>HTML body</p>\r\n" +
		"--boundary123--\r\n"

	email, err := ParseEmail(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseEmail() error: %v", err)
	}
	if email.TextBody != "Plain text body" {
		t.Errorf("TextBody = %q, want 'Plain text body'", email.TextBody)
	}
	if email.HTMLBody != "<p>HTML body</p>" {
		t.Errorf("HTMLBody = %q, want '<p>HTML body</p>'", email.HTMLBody)
	}
}

func TestParseEmailWithAttachment(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Attachment\r\n" +
		"Content-Type: multipart/mixed; boundary=mixedboundary\r\n" +
		"\r\n" +
		"--mixedboundary\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"See attached\r\n" +
		"--mixedboundary\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=\"doc.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"SGVsbG8=\r\n" +
		"--mixedboundary--\r\n"

	email, err := ParseEmail(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseEmail() error: %v", err)
	}
	if email.TextBody != "See attached" {
		t.Errorf("TextBody = %q, want 'See attached'", email.TextBody)
	}
	if len(email.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(email.Attachments))
	}
	if email.Attachments[0].Filename != "doc.pdf" {
		t.Errorf("attachment filename = %q, want doc.pdf", email.Attachments[0].Filename)
	}
	if email.Attachments[0].ContentType != "application/pdf" {
		t.Errorf("attachment content type = %q, want application/pdf", email.Attachments[0].ContentType)
	}
}

func TestParseEmailMissingContentType(t *testing.T) {
	raw := "From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: No CT\r\n\r\nPlain body"
	email, err := ParseEmail(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseEmail() error: %v", err)
	}
	if email.TextBody != "Plain body" {
		t.Errorf("TextBody = %q, want 'Plain body'", email.TextBody)
	}
}

func TestParseEmailNestedMultipart(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Subject: Nested\r\n" +
		"Content-Type: multipart/mixed; boundary=outer\r\n" +
		"\r\n" +
		"--outer\r\n" +
		"Content-Type: multipart/alternative; boundary=inner\r\n" +
		"\r\n" +
		"--inner\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Nested plain\r\n" +
		"--inner\r\n" +
		"Content-Type: text/html\r\n" +
		"\r\n" +
		"<p>Nested HTML</p>\r\n" +
		"--inner--\r\n" +
		"--outer--\r\n"

	email, err := ParseEmail(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseEmail() error: %v", err)
	}
	if email.TextBody != "Nested plain" {
		t.Errorf("TextBody = %q, want 'Nested plain'", email.TextBody)
	}
	if email.HTMLBody != "<p>Nested HTML</p>" {
		t.Errorf("HTMLBody = %q, want '<p>Nested HTML</p>'", email.HTMLBody)
	}
}

func TestDecodeBody(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		encoding string
		want     string
	}{
		{"plain", "Hello", "", "Hello"},
		{"base64", "SGVsbG8gV29ybGQ=", "base64", "Hello World"},
		{"quoted-printable", "Hello=20World", "quoted-printable", "Hello World"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeBody(strings.NewReader(tt.input), tt.encoding)
			if err != nil {
				t.Fatalf("decodeBody() error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("decodeBody() = %q, want %q", string(got), tt.want)
			}
		})
	}
}

func TestDecodeQPLine(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"hex sequences", "Hello=20World", "Hello World"},
		{"invalid hex passed through", "Hello=ZZ", "Hello=ZZ"},
		{"no encoding", "Hello World", "Hello World"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeQPLine([]byte(tt.input))
			if string(got) != tt.want {
				t.Errorf("decodeQPLine(%q) = %q, want %q", tt.input, string(got), tt.want)
			}
		})
	}
}

func TestUnhex(t *testing.T) {
	tests := []struct {
		input byte
		want  int
	}{
		{'0', 0}, {'5', 5}, {'9', 9},
		{'A', 10}, {'F', 15},
		{'a', 10}, {'f', 15},
		{'G', -1}, {'z', -1}, {' ', -1},
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			got := unhex(tt.input)
			if got != tt.want {
				t.Errorf("unhex(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestMatchRecipient(t *testing.T) {
	tests := []struct {
		name      string
		recipient string
		patterns  []string
		want      bool
	}{
		{"exact match", "user@example.com", []string{"user@example.com"}, true},
		{"glob wildcard", "user@example.com", []string{"*@example.com"}, true},
		{"no match", "user@example.com", []string{"*@other.com"}, false},
		{"case insensitive", "User@Example.COM", []string{"user@example.com"}, true},
		{"empty patterns", "user@example.com", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchRecipient(tt.recipient, tt.patterns)
			if got != tt.want {
				t.Errorf("matchRecipient(%q, %v) = %v, want %v", tt.recipient, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestParseEmailEncodedSubject(t *testing.T) {
	raw := "From: alerts@bank.example\r\nTo: me@example.com\r\n" +
		"Subject: =?UTF-8?B?RGViaXQgQWxlcnQ6IFlvdXIgSURGQyBGSVJTVCBCYW5rIENyZWRpdCBDYXJk?=\r\n\r\nbody"
	email, err := ParseEmail(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseEmail() error: %v", err)
	}
	if want := "Debit Alert: Your IDFC FIRST Bank Credit Card"; email.Subject != want {
		t.Errorf("Subject = %q, want %q", email.Subject, want)
	}
}

func TestDecodeHeader(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Plain subject", "Plain subject"},
		{"=?windows-1252?Q?Caf=E9?=", "Café"},
		{"=?gb2312?B?xOO6ww==?=", "你好"},
		{"=?unknown-charset?Q?x?=", "=?unknown-charset?Q?x?="},
	}
	for _, tt := range tests {
		if got := decodeHeader(tt.in); got != tt.want {
			t.Errorf("decodeHeader(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseAddressList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a@b.com", []string{"a@b.com"}},
		{`"Bank" <a@b.com>, c@d.com`, []string{"a@b.com", "c@d.com"}},
		{"=?windows-1252?Q?Caf=E9?= <a@b.com>", []string{"a@b.com"}},
		// Malformed headers that net/mail rejects.
		{"IDFC FIRST Bank: Alerts <alerts@bank.example>", []string{"alerts@bank.example"}},
		{"Bank@Alerts <alerts@bank.example>", []string{"alerts@bank.example"}},
		{"<alerts@bank.example>;", []string{"alerts@bank.example"}},
		{"Name: a <a@b.com>, Other: c <c@d.com>", []string{"a@b.com", "c@d.com"}},
		{"alerts@bank.example;", []string{"alerts@bank.example"}},
	}
	for _, tt := range tests {
		got := parseAddressList(tt.in)
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("parseAddressList(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// Regression: some bulk mailers emit an empty encoded-word as the display
// name, which net/mail rejects outright.
func TestParseEmailEmptyEncodedWordFrom(t *testing.T) {
	raw := "To: <idfc@example.com>\r\n" +
		"From: =?UTF-8?B??= <noreply@idfcfirstbank.com>\r\n" +
		"Subject: =?UTF-8?B?RGViaXQgQWxlcnQ6IFlvdXIgSURGQyBGSVJTVCBCYW5rIENyZWRpdCBDYXJk?=\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"_----------=_1791\"\r\n" +
		"Content-Transfer-Encoding: binary\r\n\r\n" +
		"--_----------=_1791\r\n" +
		"Content-Disposition: inline\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"Content-Type: text/plain; charset=\"utf-8\"\r\n\r\n" +
		"Transaction Successful! USD 11.09 spent on your IDFC FIRST=\r\n BANK Credit Card.\r\n" +
		"--_----------=_1791--\r\n"
	email, err := ParseEmail(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseEmail() error: %v", err)
	}
	if email.From != "noreply@idfcfirstbank.com" {
		t.Errorf("From = %q, want noreply@idfcfirstbank.com", email.From)
	}
	if len(email.To) != 1 || email.To[0] != "idfc@example.com" {
		t.Errorf("To = %v, want [idfc@example.com]", email.To)
	}
	if want := "Debit Alert: Your IDFC FIRST Bank Credit Card"; email.Subject != want {
		t.Errorf("Subject = %q, want %q", email.Subject, want)
	}
	if !strings.Contains(email.TextBody, "IDFC FIRST BANK Credit Card") {
		t.Errorf("TextBody = %q, want decoded quoted-printable", email.TextBody)
	}
}
