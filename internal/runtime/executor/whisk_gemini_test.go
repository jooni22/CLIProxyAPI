package executor

import "testing"

func TestParseGeminiImageRequest_TextOnly(t *testing.T) {
	payload := []byte(`{
		"contents":[{"parts":[{"text":"A cat in space"}]}],
		"generationConfig":{
			"candidateCount":2,
			"imageConfig":{"aspectRatio":"16:9"},
			"responseModalities":["IMAGE"]
		}
	}`)

	req, err := parseGeminiImageRequest(payload)
	if err != nil {
		t.Fatalf("parseGeminiImageRequest error: %v", err)
	}
	if req.Prompt != "A cat in space" {
		t.Fatalf("expected prompt to match, got %q", req.Prompt)
	}
	if req.CandidateCount != 2 {
		t.Fatalf("expected candidateCount 2, got %d", req.CandidateCount)
	}
	if req.AspectRatio != AspectRatioLandscape {
		t.Fatalf("expected landscape aspect ratio, got %q", req.AspectRatio)
	}
	if req.ImageBase64 != "" {
		t.Fatalf("expected empty image base64, got %q", req.ImageBase64)
	}
	if !req.WantsImage() || req.WantsText() {
		t.Fatalf("expected image-only modalities")
	}
}

func TestParseGeminiImageRequest_WithImageAndSystemInstruction(t *testing.T) {
	payload := []byte(`{
		"systemInstruction":{"parts":[{"text":"You are helpful"}]},
		"contents":[{"parts":[{"text":"Add a hat"},{"inlineData":{"mimeType":"image/png","data":"data:image/png;base64,abcd"}}]}],
		"generationConfig":{
			"candidateCount":1,
			"imageConfig":{"imageSize":"1024x1024"},
			"responseModalities":["TEXT"]
		}
	}`)

	req, err := parseGeminiImageRequest(payload)
	if err != nil {
		t.Fatalf("parseGeminiImageRequest error: %v", err)
	}
	if req.Prompt != "You are helpful\nAdd a hat" {
		t.Fatalf("expected prompt to include system instruction, got %q", req.Prompt)
	}
	if req.ImageBase64 != "abcd" {
		t.Fatalf("expected base64 data to be stripped, got %q", req.ImageBase64)
	}
	if req.AspectRatio != AspectRatioSquare {
		t.Fatalf("expected square aspect ratio, got %q", req.AspectRatio)
	}
	if req.WantsImage() || !req.WantsText() {
		t.Fatalf("expected text-only modalities")
	}
}
