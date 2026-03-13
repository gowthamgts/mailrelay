package smtp

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"

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

	// Parse From.
	if from, err := msg.Header.AddressList("From"); err == nil && len(from) > 0 {
		email.From = from[0].Address
	}

	// Parse To.
	if to, err := msg.Header.AddressList("To"); err == nil {
		for _, addr := range to {
			email.To = append(email.To, addr.Address)
		}
	}

	// Parse CC.
	if cc, err := msg.Header.AddressList("Cc"); err == nil {
		for _, addr := range cc {
			email.CC = append(email.CC, addr.Address)
		}
	}

	email.Subject = msg.Header.Get("Subject")

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
