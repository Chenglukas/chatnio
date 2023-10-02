package manager

import (
	"chat/adapter"
	"chat/auth"
	"chat/globals"
	"chat/utils"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"time"
)

type TranshipmentForm struct {
	Model     string            `json:"model" binding:"required"`
	Messages  []globals.Message `json:"messages" binding:"required"`
	Stream    bool              `json:"stream"`
	MaxTokens int               `json:"max_tokens"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      globals.Message `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type TranshipmentResponse struct {
	Id      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
	Quota   float32  `json:"quota"`
}

type ChoiceDelta struct {
	Index        int             `json:"index"`
	Delta        globals.Message `json:"delta"`
	FinishReason interface{}     `json:"finish_reason"`
}

type TranshipmentStreamResponse struct {
	Id      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChoiceDelta `json:"choices"`
	Usage   Usage         `json:"usage"`
	Quota   float32       `json:"quota"`
}

func TranshipmentAPI(c *gin.Context) {
	username := utils.GetUserFromContext(c)
	if username == "" {
		c.AbortWithStatusJSON(403, gin.H{
			"code":    403,
			"message": "Access denied. Please provide correct api key.",
		})
		return
	}

	if utils.GetAgentFromContext(c) != "api" {
		c.AbortWithStatusJSON(403, gin.H{
			"code":    403,
			"message": "Access denied. Please provide correct api key.",
		})
		return
	}

	var form TranshipmentForm
	if err := c.ShouldBindJSON(&form); err != nil {
		c.JSON(400, gin.H{
			"status": false,
			"error":  "invalid request body",
			"reason": err.Error(),
		})
		return
	}

	db := utils.GetDBFromContext(c)
	cache := utils.GetCacheFromContext(c)
	user := &auth.User{
		Username: username,
	}
	id := utils.Md5Encrypt(username + form.Model + time.Now().String())
	created := time.Now().Unix()

	reversible := globals.IsGPT4NativeModel(form.Model) && auth.CanEnableSubscription(db, cache, user)

	if !auth.CanEnableModelWithSubscription(db, user, form.Model, reversible) {
		c.JSON(http.StatusForbidden, gin.H{
			"status": false,
			"error":  "quota exceeded",
			"reason": "not enough quota to use this model",
		})
		return
	}

	if form.Stream {
		sendStreamTranshipmentResponse(c, form, id, created, user, reversible)
	} else {
		sendTranshipmentResponse(c, form, id, created, user, reversible)
	}
}

func sendTranshipmentResponse(c *gin.Context, form TranshipmentForm, id string, created int64, user *auth.User, reversible bool) {
	buffer := utils.NewBuffer(form.Model, form.Messages)
	err := adapter.NewChatRequest(&adapter.ChatProps{
		Model:      form.Model,
		Message:    form.Messages,
		Reversible: reversible && globals.IsGPT4Model(form.Model),
		Token:      form.MaxTokens,
	}, func(data string) error {
		buffer.Write(data)
		return nil
	})
	if err != nil {
		fmt.Println(fmt.Sprintf("error from chat request api: %s", err.Error()))
	}

	CollectQuota(c, user, buffer.GetQuota(), reversible)
	c.JSON(http.StatusOK, TranshipmentResponse{
		Id:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   form.Model,
		Choices: []Choice{
			{
				Index:   0,
				Message: globals.Message{Role: "assistant", Content: buffer.ReadWithDefault(defaultMessage)},
			},
		},
		Usage: Usage{
			PromptTokens:     int(buffer.CountInputToken()),
			CompletionTokens: int(buffer.CountOutputToken()),
			TotalTokens:      int(buffer.CountToken()),
		},
		Quota: buffer.GetQuota(),
	})
}

func getStreamTranshipmentForm(id string, created int64, form TranshipmentForm, data string, buffer *utils.Buffer, end bool) TranshipmentStreamResponse {
	return TranshipmentStreamResponse{
		Id:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   form.Model,
		Choices: []ChoiceDelta{
			{
				Index: 0,
				Delta: globals.Message{
					Role:    "assistant",
					Content: data,
				},
				FinishReason: utils.Multi[interface{}](end, "stop", nil),
			},
		},
		Usage: Usage{
			PromptTokens:     int(buffer.CountInputToken()),
			CompletionTokens: int(buffer.CountOutputToken()),
			TotalTokens:      int(buffer.CountToken()),
		},
		Quota: buffer.GetQuota(),
	}
}

func sendStreamTranshipmentResponse(c *gin.Context, form TranshipmentForm, id string, created int64, user *auth.User, reversible bool) {
	channel := make(chan TranshipmentStreamResponse)

	go func() {
		buffer := utils.NewBuffer(form.Model, form.Messages)
		if err := adapter.NewChatRequest(&adapter.ChatProps{
			Model:      form.Model,
			Message:    form.Messages,
			Reversible: reversible && globals.IsGPT4Model(form.Model),
			Token:      form.MaxTokens,
		}, func(data string) error {
			channel <- getStreamTranshipmentForm(id, created, form, data, buffer, false)
			return nil
		}); err != nil {
			channel <- getStreamTranshipmentForm(id, created, form, fmt.Sprintf("Error: %s", err.Error()), buffer, true)
			CollectQuota(c, user, buffer.GetQuota(), reversible)
			return
		}

		channel <- getStreamTranshipmentForm(id, created, form, "", buffer, true)
		CollectQuota(c, user, buffer.GetQuota(), reversible)
		return
	}()

	c.Stream(func(w io.Writer) bool {
		if resp, ok := <-channel; ok {
			c.SSEvent("message", resp)
			return true
		}
		return false
	})
}