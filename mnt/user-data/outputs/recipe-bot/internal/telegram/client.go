package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Telegram's own hard limits.
const (
	captionLimit = 1024 // max chars in a photo caption
	messageLimit = 4096 // max chars in a text message
)

// Client posts content to a Telegram channel via the Bot API.
type Client struct {
	token  string
	chatID string
	http   *http.Client
}

// New builds a client. chatID is either "@channelusername" or a numeric id;
// the bot must be an admin of that channel.
func New(token, chatID string) *Client {
	return &Client{
		token:  token,
		chatID: chatID,
		http:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) apiURL(method string) string {
	return fmt.Sprintf("https://api.telegram.org/bot%s/%s", c.token, method)
}

type apiResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// Publish sends a recipe with its image. If the text fits in a caption it
// goes out as a single photo+caption post; otherwise the photo is sent first
// and the full text follows as a separate message.
func (c *Client) Publish(imagePath, text string) error {
	if len([]rune(text)) <= captionLimit {
		return c.sendPhoto(imagePath, text)
	}
	if err := c.sendPhoto(imagePath, ""); err != nil {
		return err
	}
	return c.sendMessage(text)
}

// PublishText sends a text-only post (used when no image is available).
func (c *Client) PublishText(text string) error {
	return c.sendMessage(text)
}

func (c *Client) sendPhoto(imagePath, caption string) error {
	f, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("open image: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", c.chatID)
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	part, err := w.CreateFormFile("photo", filepath.Base(imagePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.apiURL("sendPhoto"), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return c.do(req)
}

// sendMessage posts text, splitting into multiple messages if it exceeds
// Telegram's per-message limit.
func (c *Client) sendMessage(text string) error {
	runes := []rune(text)
	for len(runes) > 0 {
		end := len(runes)
		if end > messageLimit {
			end = messageLimit
		}
		chunk := string(runes[:end])
		runes = runes[end:]

		form := url.Values{}
		form.Set("chat_id", c.chatID)
		form.Set("text", chunk)

		req, err := http.NewRequest(http.MethodPost, c.apiURL("sendMessage"),
			bytes.NewBufferString(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := c.do(req); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) do(req *http.Request) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("telegram decode (status %d): %s", resp.StatusCode, string(body))
	}
	if !parsed.OK {
		return fmt.Errorf("telegram api error: %s", parsed.Description)
	}
	return nil
}
