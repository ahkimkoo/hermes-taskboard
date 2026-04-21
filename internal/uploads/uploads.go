// Package uploads stores user-uploaded images either on local disk
// (served back via /uploads/{name}) or on Aliyun OSS when configured.
package uploads

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Service saves uploaded bytes and returns a URL. If OSS is configured it
// PUTs to Aliyun OSS; otherwise it stores under dataDir/uploads/ and returns
// a relative URL that the server can render.
type Service struct {
	LocalDir    string // absolute path: data/uploads/
	PublicBase  string // e.g. "/uploads" — optional prefix for local URLs

	OSSEnabled     bool
	OSSEndpoint    string
	OSSBucket      string
	OSSAccessKeyID string
	OSSSecret      string
	OSSPathPrefix  string
	OSSPublicBase  string
	HTTP           *http.Client
}

// Put stores data (with file extension derived from contentType) and returns the URL.
func (s *Service) Put(ctx context.Context, data []byte, contentType string) (string, error) {
	ext := extForType(contentType)
	name := hashName(data) + ext
	if s.OSSEnabled {
		return s.putOSS(ctx, name, data, contentType)
	}
	return s.putLocal(name, data)
}

func (s *Service) putLocal(name string, data []byte) (string, error) {
	if err := os.MkdirAll(s.LocalDir, 0o700); err != nil {
		return "", err
	}
	full := filepath.Join(s.LocalDir, name)
	if _, err := os.Stat(full); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(full, data, 0o600); err != nil {
			return "", err
		}
	}
	prefix := s.PublicBase
	if prefix == "" {
		prefix = "/uploads"
	}
	return prefix + "/" + name, nil
}

// ServeLocal serves files from LocalDir under a {name} URL suffix.
func (s *Service) ServeLocal(w http.ResponseWriter, r *http.Request, name string) {
	if strings.ContainsAny(name, "/\\") || name == "" {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.LocalDir, name)
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "stat failed", 500)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// ---- OSS ----

// putOSS uploads the blob to Aliyun OSS and returns its public URL.
// Uses the v1 PUT Object signing:
//
//   Authorization: OSS <AccessKeyID>:<Base64(HMAC-SHA1(SignString, AccessKeySecret))>
//   SignString = VERB + "\n" + Content-MD5 + "\n" + Content-Type + "\n" + Date + "\n" + CanonicalizedResource
func (s *Service) putOSS(ctx context.Context, name string, data []byte, contentType string) (string, error) {
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	objectKey := s.OSSPathPrefix + name
	host := s.OSSBucket + "." + s.OSSEndpoint
	urlStr := "https://" + host + "/" + objectKey

	date := time.Now().UTC().Format(http.TimeFormat)
	canonicalResource := "/" + s.OSSBucket + "/" + objectKey
	signStr := "PUT\n\n" + contentType + "\n" + date + "\n" + canonicalResource
	mac := hmac.New(sha1.New, []byte(s.OSSSecret))
	mac.Write([]byte(signStr))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, "PUT", urlStr, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", "OSS "+s.OSSAccessKeyID+":"+sig)
	req.ContentLength = int64(len(data))

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("oss put %s: %s", resp.Status, string(body))
	}
	publicBase := strings.TrimRight(s.OSSPublicBase, "/")
	if publicBase == "" {
		publicBase = "https://" + host
	}
	return publicBase + "/" + objectKey, nil
}

// ---- helpers ----

func hashName(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:12])
}

func extForType(ct string) string {
	// Strip ;charset=… etc.
	bare := ct
	if i := strings.IndexByte(bare, ';'); i >= 0 {
		bare = strings.TrimSpace(bare[:i])
	}
	bare = strings.ToLower(bare)
	switch bare {
	// Images
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml", "image/svg":
		return ".svg"
	// Audio
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return ".m4a"
	// Video
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "video/x-msvideo":
		return ".avi"
	case "video/webm":
		return ".webm"
	// Documents
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "application/msword":
		return ".doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.ms-excel":
		return ".xls"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.ms-powerpoint":
		return ".ppt"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	}
	return ".bin"
}

// ExtForFilename mirrors extForType but uses the original filename's
// extension when MIME info isn't precise enough (e.g. application/octet-
// stream from drag-drop on some browsers).
func ExtForFilename(name string) string {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return ""
	}
	return strings.ToLower(name[dot:])
}

