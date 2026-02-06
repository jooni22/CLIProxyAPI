package executor

import (
	"testing"

	"github.com/tidwall/gjson"
	"strings"
)

func TestWhiskExecutor_Execute_Parsing(t *testing.T) {
	// Mock request payload (Gemini format)
	payload := `{
		"contents": [
			{
				"parts": [
					{"text": "Describe this image"},
					{"inline_data": {"mime_type": "image/png", "data": "BASE64DATA"}}
				]
			}
		],
		"generationConfig": {
			"imageConfig": {
				"aspectRatio": "16:9"
			}
		}
	}`

	t.Run("heuristic_parsing", func(t *testing.T) {
		root := gjson.Parse(payload)
		var textBuilder strings.Builder
		var imageB64s []string

		root.Get("contents").ForEach(func(_, content gjson.Result) bool {
			content.Get("parts").ForEach(func(_, part gjson.Result) bool {
				if textPart := part.Get("text"); textPart.Exists() {
					if textBuilder.Len() > 0 {
						textBuilder.WriteString(" ")
					}
					textBuilder.WriteString(textPart.String())
				}
				if inlineData := part.Get("inline_data"); inlineData.Exists() {
					if data := inlineData.Get("data"); data.Exists() {
						imageB64s = append(imageB64s, data.String())
					}
				}
				return true
			})
			return true
		})

		prompt := textBuilder.String()
		if prompt != "Describe this image" {
			t.Errorf("Expected 'Describe this image', got '%s'", prompt)
		}
		if len(imageB64s) != 1 || imageB64s[0] != "BASE64DATA" {
			t.Errorf("Expected 1 image with BASE64DATA, got %v", imageB64s)
		}

		lowerPrompt := strings.ToLower(prompt)
		isDescription := strings.Contains(lowerPrompt, "describe")
		if !isDescription {
			t.Errorf("Expected isDescription to be true for prompt '%s'", prompt)
		}
	})

	t.Run("mapGeminiAspectRatioToWhisk", func(t *testing.T) {
		if mapGeminiAspectRatioToWhisk("16:9") != AspectRatioLandscape {
			t.Errorf("Expected AspectRatioLandscape, got %s", mapGeminiAspectRatioToWhisk("16:9"))
		}
		if mapGeminiAspectRatioToWhisk("1:1") != AspectRatioSquare {
			t.Errorf("Expected AspectRatioSquare, got %s", mapGeminiAspectRatioToWhisk("1:1"))
		}
		if mapGeminiAspectRatioToWhisk("9:16") != AspectRatioPortrait {
			t.Errorf("Expected AspectRatioPortrait, got %s", mapGeminiAspectRatioToWhisk("9:16"))
		}
	})

	t.Run("formatGeminiImageResponse", func(t *testing.T) {
		whiskResp := &WhiskImageResponse{
			Data: []WhiskImageData{
				{B64JSON: "IMG1", Prompt: "Prompt 1"},
			},
		}
		resp := formatGeminiImageResponse(whiskResp, "gemini-2.5-flash-image")

		root := gjson.ParseBytes(resp.Payload)
		if root.Get("model").String() != "gemini-2.5-flash-image" {
			t.Errorf("Expected model gemini-2.5-flash-image, got %s", root.Get("model").String())
		}

		parts := root.Get("candidates.0.content.parts").Array()
		if len(parts) < 1 {
			t.Fatal("Expected at least 1 part")
		}

		if parts[0].Get("inlineData.data").String() != "IMG1" {
			t.Errorf("Expected IMG1, got %s", parts[0].Get("inlineData.data").String())
		}
	})

	t.Run("formatGeminiTextResponse", func(t *testing.T) {
		resp := formatGeminiTextResponse("Hello", "gemini-2.5-flash-image")
		root := gjson.ParseBytes(resp.Payload)
		if root.Get("candidates.0.content.parts.0.text").String() != "Hello" {
			t.Errorf("Expected Hello, got %s", root.Get("candidates.0.content.parts.0.text").String())
		}
	})
}
