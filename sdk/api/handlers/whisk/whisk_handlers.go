package whisk

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"

	"github.com/google/uuid"
)

type WhiskAPIHandler struct {
	*handlers.BaseAPIHandler
	whiskExecutor *executor.WhiskExecutor
}

func NewWhiskAPIHandler(apiHandlers *handlers.BaseAPIHandler, cfg *config.Config) *WhiskAPIHandler {
	return &WhiskAPIHandler{
		BaseAPIHandler: apiHandlers,
		whiskExecutor:  executor.NewWhiskExecutor(cfg),
	}
}

func NewWhiskAPIHandlerFromSDKConfig(apiHandlers *handlers.BaseAPIHandler, cfg *sdkconfig.SDKConfig) *WhiskAPIHandler {
	return &WhiskAPIHandler{
		BaseAPIHandler: apiHandlers,
		whiskExecutor:  executor.NewWhiskExecutor(nil),
	}
}

func (h *WhiskAPIHandler) HandlerType() string {
	return "whisk"
}

type ImageGenerationRequest struct {
	Model          string `json:"model,omitempty"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	Seed           int    `json:"seed,omitempty"`
}

type ImageGenerationResponse struct {
	Created int64       `json:"created"`
	Data    []ImageData `json:"data"`
}

type ImageData struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// ImageGenerations godoc
// @Summary      Generate images from text prompt
// @Description  Generate images using Whisk's text-to-image models (IMAGEN_3_5 or GEM_PIX)
// @Tags         whisk
// @Accept       json
// @Produce      json
// @Param        request body ImageGenerationRequest true "Image generation request"
// @Success      200 {object} ImageGenerationResponse
// @Failure      400 {object} object{error=object{message=string,type=string,code=string}}
// @Failure      401 {object} object{error=object{message=string}}
// @Security     BearerAuth
// @Router       /v1/images/generations [post]
func (h *WhiskAPIHandler) ImageGenerations(c *gin.Context) {
	rawJSON, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "Failed to read request body",
				"type":    "invalid_request_error",
				"code":    "invalid_request",
			},
		})
		return
	}

	var req ImageGenerationRequest
	if err := json.Unmarshal(rawJSON, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "Invalid JSON: " + err.Error(),
				"type":    "invalid_request_error",
				"code":    "invalid_json",
			},
		})
		return
	}

	if strings.TrimSpace(req.Prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "prompt is required",
				"type":    "invalid_request_error",
				"code":    "missing_prompt",
			},
		})
		return
	}

	auth := h.getWhiskAuth()
	if auth == nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "No Whisk authentication available. Use --whisk-cookie to configure.",
				"type":    "authentication_error",
				"code":    "no_auth",
			},
		})
		return
	}

	if h.whiskExecutor == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Whisk executor not available",
				"type":    "server_error",
				"code":    "executor_unavailable",
			},
		})
		return
	}

	model := req.Model
	if model == "" {
		model = "IMAGEN_3_5"
	}
	model = normalizeWhiskModel(model)

	aspectRatio := sizeToAspectRatio(req.Size)
	n := req.N
	if n <= 0 {
		n = 1
	}
	if n > 4 {
		n = 4
	}

	whiskReq := executor.WhiskImageRequest{
		Prompt:      req.Prompt,
		Model:       model,
		AspectRatio: aspectRatio,
		NumImages:   n,
		Seed:        req.Seed,
	}

	ctx := c.Request.Context()
	result, err := h.whiskExecutor.GenerateImage(ctx, auth, whiskReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": err.Error(),
				"type":    "api_error",
				"code":    "generation_failed",
			},
		})
		return
	}

	responseFormat := req.ResponseFormat
	if responseFormat == "" {
		responseFormat = "b64_json"
	}

	resp := ImageGenerationResponse{
		Created: time.Now().Unix(),
		Data:    make([]ImageData, 0, len(result.Data)),
	}

	for _, img := range result.Data {
		data := ImageData{
			RevisedPrompt: img.Prompt,
		}
		if responseFormat == "url" {
			data.URL = img.B64JSON
		} else {
			b64 := img.B64JSON
			if idx := strings.Index(b64, ","); idx > 0 {
				b64 = b64[idx+1:]
			}
			data.B64JSON = b64
		}
		resp.Data = append(resp.Data, data)
	}

	// Publish usage record
	h.publishUsage(c, auth, "whisk", req.Model)

	c.JSON(http.StatusOK, resp)
}

// RefineImage godoc
// @Summary      Refine/edit an existing image
// @Description  Edit an image using Whisk GEM_PIX model with optional Smart Refine (auto-caption enhancement)
// @Tags         whisk
// @Accept       json
// @Produce      json
// @Param        request body object{image=string,instruction=string,original_prompt=string,model=string,aspect_ratio=string,enhance_context=boolean} true "Refine request. Only 'image' is required."
// @Success      200 {object} ImageGenerationResponse
// @Failure      400 {object} object{error=object{message=string}}
// @Failure      401 {object} object{error=object{message=string}}
// @Security     BearerAuth
// @Router       /v1/whisk/refine [post]
func (h *WhiskAPIHandler) RefineImage(c *gin.Context) {
	rawJSON, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Failed to read request body"}})
		return
	}

	var req struct {
		Image          string `json:"image"`
		Instruction    string `json:"instruction,omitempty"`
		OriginalPrompt string `json:"original_prompt,omitempty"`
		Model          string `json:"model,omitempty"`
		AspectRatio    string `json:"aspect_ratio,omitempty"`
		EnhanceContext bool   `json:"enhance_context,omitempty"`
	}
	if err := json.Unmarshal(rawJSON, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid JSON: " + err.Error()}})
		return
	}

	// Only image is required - instruction has a forgiving default
	if strings.TrimSpace(req.Image) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "image is required"}})
		return
	}

	// Apply forgiving default for instruction
	instruction := req.Instruction
	if strings.TrimSpace(instruction) == "" {
		instruction = "enhance image quality"
	}

	auth := h.getWhiskAuth()
	if auth == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "No Whisk authentication available"}})
		return
	}

	ctx := c.Request.Context()
	result, err := h.whiskExecutor.RefineImage(ctx, auth, req.Image, instruction, req.OriginalPrompt, req.Model, req.AspectRatio, req.EnhanceContext)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}

	resp := ImageGenerationResponse{
		Created: time.Now().Unix(),
		Data:    make([]ImageData, 0, len(result.Data)),
	}

	for _, img := range result.Data {
		b64 := img.B64JSON
		if idx := strings.Index(b64, ","); idx > 0 {
			b64 = b64[idx+1:]
		}
		resp.Data = append(resp.Data, ImageData{
			B64JSON:       b64,
			RevisedPrompt: img.Prompt,
		})
	}

	// Publish usage record
	h.publishUsage(c, auth, "whisk", "gemini-2.5-flash-image")

	c.JSON(http.StatusOK, resp)
}

// UploadImage godoc
// @Summary      Upload an image to Whisk
// @Description  Upload an image to get a mediaGenerationId for use in workflows
// @Tags         whisk
// @Accept       json
// @Produce      json
// @Param        request body object{image=string,caption=string} true "Upload request. 'image' is required."
// @Success      200 {object} object{uploadMediaGenerationId=string}
// @Failure      400 {object} object{error=object{message=string}}
// @Failure      401 {object} object{error=object{message=string}}
// @Security     BearerAuth
// @Router       /v1/whisk/upload [post]
func (h *WhiskAPIHandler) UploadImage(c *gin.Context) {
	rawJSON, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Failed to read request body"}})
		return
	}

	var req struct {
		Image   string `json:"image"`
		Caption string `json:"caption,omitempty"`
	}
	if err := json.Unmarshal(rawJSON, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid JSON: " + err.Error()}})
		return
	}

	if strings.TrimSpace(req.Image) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "image is required"}})
		return
	}

	auth := h.getWhiskAuth()
	if auth == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "No Whisk authentication available"}})
		return
	}

	ctx := c.Request.Context()
	workflowID := uuid.New().String()

	mediaID, err := h.whiskExecutor.UploadImage(ctx, auth, req.Image, req.Caption, workflowID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}

	// Publish usage record
	h.publishUsage(c, auth, "whisk", "whisk-upload")

	c.JSON(http.StatusOK, gin.H{
		"uploadMediaGenerationId": mediaID,
	})
}

// GenerateCaption godoc
// @Summary      Generate caption for an image
// @Description  Analyze an image and generate a descriptive caption using Whisk's captioning model
// @Tags         whisk
// @Accept       json
// @Produce      json
// @Param        request body object{image=string,count=integer} true "Caption request. 'image' is required, 'count' defaults to 1."
// @Success      200 {object} object{captions=[]string}
// @Failure      400 {object} object{error=object{message=string}}
// @Failure      401 {object} object{error=object{message=string}}
// @Security     BearerAuth
// @Router       /v1/whisk/caption [post]
func (h *WhiskAPIHandler) GenerateCaption(c *gin.Context) {
	rawJSON, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Failed to read request body"}})
		return
	}

	var req struct {
		Image string `json:"image"`
		Count int    `json:"count,omitempty"`
	}
	if err := json.Unmarshal(rawJSON, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid JSON: " + err.Error()}})
		return
	}

	if strings.TrimSpace(req.Image) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "image is required"}})
		return
	}

	auth := h.getWhiskAuth()
	if auth == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "No Whisk authentication available"}})
		return
	}

	// Apply forgiving default for count
	count := req.Count
	if count <= 0 {
		count = 1
	}

	ctx := c.Request.Context()
	captions, err := h.whiskExecutor.GenerateCaption(ctx, auth, req.Image, count)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}

	// Publish usage record
	h.publishUsage(c, auth, "whisk", "whisk-caption")

	c.JSON(http.StatusOK, gin.H{
		"captions": captions,
	})
}

// publishUsage emits a usage record for Whisk API calls
func (h *WhiskAPIHandler) publishUsage(c *gin.Context, auth *cliproxyauth.Auth, provider, model string) {
	apiKey := ""
	if v, exists := c.Get("apiKey"); exists {
		if key, ok := v.(string); ok {
			apiKey = key
		}
	}

	authID := ""
	authIndex := ""
	source := ""
	if auth != nil {
		authID = auth.ID
		authIndex = auth.EnsureIndex()
		if auth.Metadata != nil {
			if email, ok := auth.Metadata["email"].(string); ok {
				source = email
			}
		}
	}

	usage.PublishRecord(c.Request.Context(), usage.Record{
		Provider:    provider,
		Model:       model,
		Source:      source,
		APIKey:      apiKey,
		AuthID:      authID,
		AuthIndex:   authIndex,
		RequestedAt: time.Now(),
		Failed:      false,
		Detail:      usage.Detail{TotalTokens: 1}, // Count as 1 "token" for image operations
	})
}

func (h *WhiskAPIHandler) getWhiskAuth() *cliproxyauth.Auth {
	if h.BaseAPIHandler == nil || h.AuthManager == nil {
		return nil
	}
	allAuths := h.AuthManager.List()
	for _, auth := range allAuths {
		if auth != nil && strings.EqualFold(auth.Provider, "whisk") {
			return auth
		}
	}
	return nil
}

func (h *WhiskAPIHandler) GenerateImageDirect(ctx context.Context, auth *cliproxyauth.Auth, req executor.WhiskImageRequest) (*executor.WhiskImageResponse, error) {
	return h.whiskExecutor.GenerateImage(ctx, auth, req)
}

func normalizeWhiskModel(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(lower, "gem_pix") || strings.Contains(lower, "gempix"):
		return "GEM_PIX"
	case strings.Contains(lower, "imagen") || strings.Contains(lower, "3_5") || strings.Contains(lower, "3.5"):
		return "IMAGEN_3_5"
	case strings.Contains(lower, "r2i"):
		return "R2I"
	default:
		return "GEM_PIX"
	}
}

func sizeToAspectRatio(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1024x1024", "square", "1:1":
		return "IMAGE_ASPECT_RATIO_SQUARE"
	case "1024x1792", "portrait", "9:16":
		return "IMAGE_ASPECT_RATIO_PORTRAIT"
	case "1792x1024", "landscape", "16:9":
		return "IMAGE_ASPECT_RATIO_LANDSCAPE"
	default:
		return "IMAGE_ASPECT_RATIO_PORTRAIT"
	}
}
