package imap

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime/quotedprintable"
	"strings"

	"github.com/emersion/go-imap"
)

// FetchDecodedAttachment fetches one attachment from a selected mailbox
// by UID, resolves its IMAP section path via BODYSTRUCTURE, and streams
// the decoded payload (base64 / quoted-printable / 7bit / 8bit / binary)
// to w.
//
// Matching is tried in order:
//  1. By filename (Content-Disposition filename or Content-Type name)
//  2. By 1-based attachment index, counting attachment-like leaves in
//     DFS order.
//
// The caller is expected to have already selected the mailbox.
func (c *Client) FetchDecodedAttachment(uid uint32, filename string, partIndex int, w io.Writer) error {
	bs, err := c.FetchBodyStructure(uid)
	if err != nil {
		return fmt.Errorf("fetch BODYSTRUCTURE: %w", err)
	}
	if bs == nil {
		return fmt.Errorf("no message found for UID %d", uid)
	}

	sectionPath, encoding := FindAttachmentSection(bs, filename, partIndex)
	if sectionPath == nil {
		return fmt.Errorf("attachment %q not found in BODYSTRUCTURE", filename)
	}

	return c.FetchAndDecodeBodySection(uid, sectionPath, encoding, w)
}

// FetchAndDecodeBodySection issues FETCH BODY[section] and pipes the raw
// transfer-encoded bytes through a decoder matching the Content-Transfer-
// Encoding from BODYSTRUCTURE.
func (c *Client) FetchAndDecodeBodySection(uid uint32, sectionPath []int, encoding string, dest io.Writer) error {
	switch strings.ToLower(encoding) {
	case "base64":
		pr, pw := io.Pipe()
		defer pr.Close()
		go func() {
			err := c.FetchBodySection(uid, sectionPath, pw)
			pw.CloseWithError(err)
		}()
		decoder := base64.NewDecoder(base64.StdEncoding, pr)
		_, err := io.Copy(dest, decoder)
		return err
	case "quoted-printable":
		pr, pw := io.Pipe()
		defer pr.Close()
		go func() {
			err := c.FetchBodySection(uid, sectionPath, pw)
			pw.CloseWithError(err)
		}()
		_, err := io.Copy(dest, quotedprintable.NewReader(pr))
		return err
	default:
		// 7bit, 8bit, binary — no decoding needed
		return c.FetchBodySection(uid, sectionPath, dest)
	}
}

// FindAttachmentSection walks an IMAP BODYSTRUCTURE tree and returns the
// section path (e.g. []int{3} or []int{2,1}) and Content-Transfer-Encoding
// for the target attachment.
//
// Uses dual matching:
//  1. Primary: match by filename (DispositionParams["filename"] or Params["name"])
//  2. Fallback: match by 1-based attachment index, counting only
//     attachment-like leaves in DFS order.
func FindAttachmentSection(bs *imap.BodyStructure, filename string, partIndex int) ([]int, string) {
	if path, enc := walkForFilename(bs, filename, nil); path != nil {
		return path, enc
	}
	counter := 0
	if path, enc := walkForIndex(bs, partIndex, &counter, nil); path != nil {
		return path, enc
	}
	return nil, ""
}

// walkForFilename does a DFS looking for a leaf whose filename matches target.
func walkForFilename(bs *imap.BodyStructure, target string, prefix []int) ([]int, string) {
	if len(bs.Parts) > 0 {
		for i, child := range bs.Parts {
			path, enc := walkForFilename(child, target, append(append([]int{}, prefix...), i+1))
			if path != nil {
				return path, enc
			}
		}
		return nil, ""
	}
	name := bs.DispositionParams["filename"]
	if name == "" {
		name = bs.Params["name"]
	}
	if target != "" && strings.EqualFold(name, target) {
		return prefix, bs.Encoding
	}
	return nil, ""
}

// walkForIndex does a DFS counting attachment-like leaves. Returns the
// section path and encoding when the counter reaches partIndex.
//
// For a single-part top-level message (no Parts) that itself looks like an
// attachment (e.g. Google's application/zip DMARC reports), the section
// path is []int{1} per RFC 3501 §6.4.5.
func walkForIndex(bs *imap.BodyStructure, partIndex int, counter *int, prefix []int) ([]int, string) {
	if len(bs.Parts) > 0 {
		for i, child := range bs.Parts {
			path, enc := walkForIndex(child, partIndex, counter, append(append([]int{}, prefix...), i+1))
			if path != nil {
				return path, enc
			}
		}
		return nil, ""
	}
	if !IsAttachmentLike(bs) {
		return nil, ""
	}
	*counter++
	if *counter == partIndex {
		// Top-level non-multipart message: section path is [1].
		if len(prefix) == 0 {
			return []int{1}, bs.Encoding
		}
		return prefix, bs.Encoding
	}
	return nil, ""
}

// IsAttachmentLike returns true if the BODYSTRUCTURE leaf looks like an
// attachment: has disposition "attachment", or has a filename and isn't text/*.
func IsAttachmentLike(bs *imap.BodyStructure) bool {
	if strings.EqualFold(bs.Disposition, "attachment") {
		return true
	}
	name := bs.DispositionParams["filename"]
	if name == "" {
		name = bs.Params["name"]
	}
	return name != "" && !strings.EqualFold(bs.MIMEType, "text")
}
