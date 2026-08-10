package llm

import "context"

// Client is a minimal LLM client stub for the scaffold.
type Client struct {
    ServerURL string
    Model     string
}

// NewClient constructs a new Client.
func NewClient(serverURL, model string) *Client {
    return &Client{ServerURL: serverURL, Model: model}
}

// Chat sends input to the model and returns a short stubbed response.
func (c *Client) Chat(ctx context.Context, input string) (string, error) {
    return "[stub reply] " + input, nil
}
