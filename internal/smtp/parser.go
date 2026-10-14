package smtp

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"regexp"
	"strings"

	"golang.org/x/text/encoding/htmlindex"

	"github.com/gowthamgts/mailrelay/internal/models"
)

// ParseEmail reads a raw email message and returns a ParsedEmail.
func ParseEmail(r io.Reader) (*models.ParsedEmail, error) {
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return nil, fmt.Errorf("reading message: %w", err)
	}

	email := &models.ParsedEmail{
		Headers: msg.Header,
	}

	if from := parseAddressList(msg.Header.Get("From")); len(from) > 0 {
		email.From = from[0]
	}
	email.To = parseAddressList(msg.Header.Get("To"))
	email.CC = parseAddressList(msg.Header.Get("Cc"))
	email.Subject = decodeHeader(msg.Header.Get("Subject"))

	contentType := msg.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// Treat as plain text on parse failure.
		body, _ := io.ReadAll(msg.Body)
		email.TextBody = string(body)
		return email, nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		if err := parseMultipart(msg.Body, params["boundary"], email); err != nil {
			return nil, fmt.Errorf("parsing multipart: %w", err)
		}
	} else {
		body, err := decodeBody(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			return nil, fmt.Errorf("decoding body: %w", err)
		}
		if strings.HasPrefix(mediaType, "text/html") {
			email.HTMLBody = string(body)
		} else {
			email.TextBody = string(body)
		}
	}

	return email, nil
}

// headerDecoder decodes RFC 2047 encoded-words in any charset known to the
// WHATWG encoding spec, not just the UTF-8/US-ASCII/ISO-8859-1 trio that the
// standard library handles on its own.
var headerDecoder = &mime.WordDecoder{
	CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
		enc, err := htmlindex.Get(charset)
		if err != nil {
			return nil, err
		}
		return enc.NewDecoder().Reader(input), nil
	},
}

// decodeHeader decodes RFC 2047 encoded-words, falling back to the raw value.
func decodeHeader(v string) string {
	decoded, err := headerDecoder.DecodeHeader(v)
	if err != nil {
		return v
	}
	return decoded
}

var addressParser = &mail.AddressParser{WordDecoder: headerDecoder}

// looseAddrRe matches anything that looks like an email address.
var looseAddrRe = regexp.MustCompile(`[^\s<>()\[\]",;:@]+@[^\s<>()\[\]",;:@]+`)

// parseAddressList returns the addresses in an address header. Real-world
// senders often emit headers that violate RFC 5322 (unquoted special
// characters in display names, unknown charsets, trailing junk), so when strict
// parsing fails we fall back to extracting anything that looks like an address,
// preferring the angle-bracketed ones.
func parseAddressList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}

	var out []string
	if list, err := addressParser.ParseList(v); err == nil {
		for _, addr := range list {
			out = append(out, addr.Address)
		}
		return out
	}

	for _, seg := range strings.Split(v, ">") {
		if i := strings.LastIndex(seg, "<"); i >= 0 {
			if m := looseAddrRe.FindString(seg[i+1:]); m != "" {
				out = append(out, m)
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	return looseAddrRe.FindAllString(v, -1)
}

func parseMultipart(r io.Reader, boundary string, email *models.ParsedEmail) error {
	mr := multipart.NewReader(r, boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		partContentType := part.Header.Get("Content-Type")
		if partContentType == "" {
			partContentType = "text/plain"
		}

		mediaType, params, err := mime.ParseMediaType(partContentType)
		if err != nil {
			continue
		}

		// Handle nested multipart.
		if strings.HasPrefix(mediaType, "multipart/") {
			if err := parseMultipart(part, params["boundary"], email); err != nil {
				return err
			}
			continue
		}

		disposition := part.Header.Get("Content-Disposition")
		filename := part.FileName()
		if filename == "" && disposition != "" {
			_, dparams, _ := mime.ParseMediaType(disposition)
			filename = dparams["filename"]
		}

		transferEncoding := part.Header.Get("Content-Transfer-Encoding")

		// Attachment: has a filename or explicit attachment disposition.
		if filename != "" || strings.HasPrefix(disposition, "attachment") {
			data, err := decodeBody(part, transferEncoding)
			if err != nil {
				continue
			}
			email.Attachments = append(email.Attachments, models.Attachment{
				Filename:    filename,
				ContentType: mediaType,
				Content:     base64.StdEncoding.EncodeToString(data),
			})
			continue
		}

		// Inline text or HTML body.
		body, err := decodeBody(part, transferEncoding)
		if err != nil {
			continue
		}

		switch {
		case strings.HasPrefix(mediaType, "text/html"):
			email.HTMLBody = string(body)
		case strings.HasPrefix(mediaType, "text/plain"):
			email.TextBody = string(body)
		default:
			// Unknown inline part, store as attachment.
			email.Attachments = append(email.Attachments, models.Attachment{
				Filename:    filename,
				ContentType: mediaType,
				Content:     base64.StdEncoding.EncodeToString(body),
			})
		}
	}
}

func decodeBody(r io.Reader, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		decoded := base64.NewDecoder(base64.StdEncoding, r)
		return io.ReadAll(decoded)
	case "quoted-printable":
		return io.ReadAll(newQuotedPrintableReader(r))
	default:
		return io.ReadAll(r)
	}
}

// newQuotedPrintableReader returns a reader that decodes quoted-printable content.
func newQuotedPrintableReader(r io.Reader) io.Reader {
	// Read all data first, then decode.
	data, err := io.ReadAll(r)
	if err != nil {
		return strings.NewReader("")
	}

	var buf bytes.Buffer
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		line = bytes.TrimRight(line, "\r")

		// Handle soft line breaks.
		if bytes.HasSuffix(line, []byte("=")) {
			line = line[:len(line)-1]
			buf.Write(decodeQPLine(line))
			continue
		}

		buf.Write(decodeQPLine(line))
		if i < len(lines)-1 {
			buf.WriteByte('\n')
		}
	}
	return bytes.NewReader(buf.Bytes())
}

func decodeQPLine(line []byte) []byte {
	var out bytes.Buffer
	for i := 0; i < len(line); i++ {
		if line[i] == '=' && i+2 < len(line) {
			hi := unhex(line[i+1])
			lo := unhex(line[i+2])
			if hi >= 0 && lo >= 0 {
				out.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		out.WriteByte(line[i])
	}
	return out.Bytes()
}

func unhex(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	default:
		return -1
	}
}
