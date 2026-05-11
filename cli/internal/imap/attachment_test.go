package imap

import (
	"testing"

	goImap "github.com/emersion/go-imap"
)

// --- IsAttachmentLike ---

func TestIsAttachmentLike_ExplicitDisposition(t *testing.T) {
	bs := &goImap.BodyStructure{Disposition: "attachment"}
	if !IsAttachmentLike(bs) {
		t.Error("explicit attachment disposition not detected")
	}
}

func TestIsAttachmentLike_DispositionParamsFilename(t *testing.T) {
	bs := &goImap.BodyStructure{
		MIMEType:          "application",
		MIMESubType:       "pdf",
		DispositionParams: map[string]string{"filename": "report.pdf"},
	}
	if !IsAttachmentLike(bs) {
		t.Error("filename in disposition params not detected")
	}
}

func TestIsAttachmentLike_ParamsName(t *testing.T) {
	bs := &goImap.BodyStructure{
		MIMEType: "image",
		Params:   map[string]string{"name": "photo.jpg"},
	}
	if !IsAttachmentLike(bs) {
		t.Error("name in params not detected")
	}
}

func TestIsAttachmentLike_TextPartWithNameNotAttachment(t *testing.T) {
	// A text/plain part with just a name shouldn't count as attachment
	bs := &goImap.BodyStructure{
		MIMEType: "text",
		Params:   map[string]string{"name": "inline.txt"},
	}
	if IsAttachmentLike(bs) {
		t.Error("text part with name incorrectly flagged as attachment")
	}
}

func TestIsAttachmentLike_NoDispositionNoName(t *testing.T) {
	bs := &goImap.BodyStructure{MIMEType: "text", MIMESubType: "plain"}
	if IsAttachmentLike(bs) {
		t.Error("bare text part flagged as attachment")
	}
}

// --- walkForFilename ---
//
// Note: walkForFilename returns `prefix` (the accumulated path) when a leaf
// matches. Called with prefix=nil and a top-level leaf, it returns a nil
// path — which FindAttachmentSection then treats as "no filename match,
// fall through to index lookup". Tests below always use multipart roots so
// the returned path is non-nil when a match occurs.

func TestWalkForFilename_NestedMultipart(t *testing.T) {
	// multipart/mixed                   -> root
	//   [1] text/plain
	//   [2] multipart/alternative
	//     [2,1] text/plain
	//     [2,2] application/pdf report.pdf
	bs := &goImap.BodyStructure{
		MIMEType:    "multipart",
		MIMESubType: "mixed",
		Parts: []*goImap.BodyStructure{
			{MIMEType: "text", MIMESubType: "plain"},
			{
				MIMEType:    "multipart",
				MIMESubType: "alternative",
				Parts: []*goImap.BodyStructure{
					{MIMEType: "text", MIMESubType: "plain"},
					{
						MIMEType:          "application",
						MIMESubType:       "pdf",
						DispositionParams: map[string]string{"filename": "report.pdf"},
						Encoding:          "base64",
					},
				},
			},
		},
	}
	path, enc := walkForFilename(bs, "report.pdf", nil)
	if len(path) != 2 || path[0] != 2 || path[1] != 2 {
		t.Errorf("path = %v, want [2 2]", path)
	}
	if enc != "base64" {
		t.Errorf("encoding = %q", enc)
	}
}

func TestWalkForFilename_CaseInsensitive(t *testing.T) {
	// Wrap the leaf in a multipart so walkForFilename returns a non-nil
	// path on match (see note above).
	bs := &goImap.BodyStructure{
		MIMEType:    "multipart",
		MIMESubType: "mixed",
		Parts: []*goImap.BodyStructure{
			{
				MIMEType:          "application",
				MIMESubType:       "pdf",
				DispositionParams: map[string]string{"filename": "Report.PDF"},
				Encoding:          "base64",
			},
		},
	}
	path, _ := walkForFilename(bs, "report.pdf", nil)
	if len(path) != 1 || path[0] != 1 {
		t.Errorf("case-insensitive match: path = %v, want [1]", path)
	}
}

func TestWalkForFilename_NoMatch(t *testing.T) {
	bs := &goImap.BodyStructure{
		MIMEType:          "application",
		MIMESubType:       "pdf",
		DispositionParams: map[string]string{"filename": "other.pdf"},
	}
	path, _ := walkForFilename(bs, "missing.pdf", nil)
	if path != nil {
		t.Errorf("unexpected match: %v", path)
	}
}

// --- walkForIndex ---

func TestWalkForIndex_SkipsTextParts(t *testing.T) {
	// multipart/mixed
	//   text/plain
	//   application/pdf (1st attachment)
	//   image/png (2nd attachment)
	bs := &goImap.BodyStructure{
		MIMEType:    "multipart",
		MIMESubType: "mixed",
		Parts: []*goImap.BodyStructure{
			{MIMEType: "text", MIMESubType: "plain"},
			{
				MIMEType:          "application",
				MIMESubType:       "pdf",
				DispositionParams: map[string]string{"filename": "a.pdf"},
				Encoding:          "base64",
			},
			{
				MIMEType:    "image",
				MIMESubType: "png",
				Params:      map[string]string{"name": "b.png"},
				Encoding:    "base64",
			},
		},
	}

	counter := 0
	path, _ := walkForIndex(bs, 1, &counter, nil)
	if len(path) != 1 || path[0] != 2 {
		t.Errorf("1st attachment path = %v, want [2]", path)
	}

	counter = 0
	path, _ = walkForIndex(bs, 2, &counter, nil)
	if len(path) != 1 || path[0] != 3 {
		t.Errorf("2nd attachment path = %v, want [3]", path)
	}
}

func TestWalkForIndex_IndexOutOfRange(t *testing.T) {
	bs := &goImap.BodyStructure{
		MIMEType:          "application",
		MIMESubType:       "pdf",
		DispositionParams: map[string]string{"filename": "a.pdf"},
	}
	counter := 0
	path, _ := walkForIndex(bs, 5, &counter, nil)
	if path != nil {
		t.Errorf("expected nil for out-of-range index, got %v", path)
	}
}

// --- FindAttachmentSection ---

func TestFindAttachmentSection_FilenameFirst(t *testing.T) {
	// Filename "b.pdf" should match even though it's the 2nd attachment
	// (filename lookup takes precedence over index fallback).
	bs := &goImap.BodyStructure{
		MIMEType:    "multipart",
		MIMESubType: "mixed",
		Parts: []*goImap.BodyStructure{
			{
				MIMEType:          "application",
				MIMESubType:       "pdf",
				DispositionParams: map[string]string{"filename": "a.pdf"},
				Encoding:          "base64",
			},
			{
				MIMEType:          "application",
				MIMESubType:       "pdf",
				DispositionParams: map[string]string{"filename": "b.pdf"},
				Encoding:          "base64",
			},
		},
	}
	path, _ := FindAttachmentSection(bs, "b.pdf", 99)
	if len(path) != 1 || path[0] != 2 {
		t.Errorf("path = %v, want [2]", path)
	}
}

func TestFindAttachmentSection_IndexFallback(t *testing.T) {
	// Wrong filename, but index=1 still resolves to first attachment.
	bs := &goImap.BodyStructure{
		MIMEType:    "multipart",
		MIMESubType: "mixed",
		Parts: []*goImap.BodyStructure{
			{
				MIMEType:          "application",
				MIMESubType:       "pdf",
				DispositionParams: map[string]string{"filename": "a.pdf"},
				Encoding:          "base64",
			},
		},
	}
	path, _ := FindAttachmentSection(bs, "does-not-exist.pdf", 1)
	if len(path) != 1 || path[0] != 1 {
		t.Errorf("path = %v, want [1]", path)
	}
}

func TestFindAttachmentSection_NoMatch(t *testing.T) {
	bs := &goImap.BodyStructure{
		MIMEType:    "text",
		MIMESubType: "plain",
	}
	path, enc := FindAttachmentSection(bs, "a.pdf", 1)
	if path != nil || enc != "" {
		t.Errorf("expected no match, got path=%v enc=%q", path, enc)
	}
}

// --- WatcherManager construction ---
