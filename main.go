package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/invopop/jsonschema"
	"github.com/sashabaranov/go-openai"
)

// ============================================================================
// TYPES & AGENT DEFINITION
// ============================================================================

type ToolDefinition struct {
	Name        string
	Description string
	InputSchema any
	Function    func(input json.RawMessage) (string, error)
}

type Agent struct {
	client         *openai.Client
	model          string
	getUserMessage func() (string, bool)
	tools          []ToolDefinition
}

func NewAgent(
	client *openai.Client,
	model string,
	getUserMessage func() (string, bool),
	tools []ToolDefinition,
) *Agent {
	return &Agent{
		client:         client,
		model:          model,
		getUserMessage: getUserMessage,
		tools:          tools,
	}
}

// ============================================================================
// MAIN LOOP
// ============================================================================
func main() {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY") // Fallback
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	model := os.Getenv("OPENAI_MODEL")

	// Fallback defaults for Google Gemini
	if model == "" {
		model = "gemini-2.5-flash-lite"
	}
	if baseURL == "" {
		// Google's official OpenAI-compatible endpoint
		baseURL = "https://generativelanguage.googleapis.com/v1beta/openai/"
	}
	if apiKey == "" {
		fmt.Println("\033[91mWarning: GEMINI_API_KEY environment variable is not set.\033[0m")
	}

	config := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		config.BaseURL = baseURL
	}
	client := openai.NewClientWithConfig(config)

	scanner := bufio.NewScanner(os.Stdin)
	getUserMessage := func() (string, bool) {
		if !scanner.Scan() {
			return "", false
		}
		return scanner.Text(), true
	}

	// Register all our tools here
	tools := []ToolDefinition{
		ReadFileDefinition,
		ListFilesDefinition,
		EditFileDefinition,
		ExecuteCommandDefinition,
	}
	
	agent := NewAgent(client, model, getUserMessage, tools)

	err := agent.Run(context.TODO())
	if err != nil {
		fmt.Printf("\033[91mFatal Error: %s\033[0m\n", err.Error())
	}
}

func (a *Agent) Run(ctx context.Context) error {
	// Initialize with a strong system prompt to guide behavior
	conversation := []openai.ChatCompletionMessage{
		{
			Role: openai.ChatMessageRoleSystem,
			Content: `You are an elite, autonomous software engineering AI. 
You have access to tools to interact with the local filesystem and terminal.
When asked to build or modify something:
1. Explore the environment first if needed (list_files, read_file).
2. Make exact, careful edits using edit_file. 
3. Always verify your work by running the code using execute_command if applicable.
Keep your conversational responses brief, and let your actions via tools do the talking.`,
		},
	}

	fmt.Printf("\033[95mAgent initialized with model %s. Chat away! (use 'ctrl-c' to quit)\033[0m\n", a.model)

	readUserInput := true
	for {
		if readUserInput {
			fmt.Print("\033[94mYou\033[0m: ")
			userInput, ok := a.getUserMessage()
			if !ok {
				break
			}

			userMessage := openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: userInput,
			}
			conversation = append(conversation, userMessage)
		}

		resp, err := a.runInference(ctx, conversation)
		if err != nil {
			return fmt.Errorf("API error: %w", err)
		}
		if len(resp.Choices) == 0 {
			return fmt.Errorf("empty choices in response")
		}

		assistantMessage := resp.Choices[0].Message
		conversation = append(conversation, assistantMessage)

		if len(assistantMessage.Content) > 0 {
			fmt.Printf("\033[93mAI\033[0m: %s\n", assistantMessage.Content)
		}

		// If no tools were requested, wait for user input again
		if len(assistantMessage.ToolCalls) == 0 {
			readUserInput = true
			continue
		}

		// Execute all requested tools sequentially
		for _, toolCall := range assistantMessage.ToolCalls {
			resultStr := a.executeTool(toolCall.ID, toolCall.Function.Name, json.RawMessage(toolCall.Function.Arguments))
			
			// Protect context window from massive outputs
			if len(resultStr) > 10000 {
				resultStr = resultStr[:10000] + "\n\n...[OUTPUT TRUNCATED FOR LENGTH]..."
			}

			conversation = append(conversation, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    resultStr,
				Name:       toolCall.Function.Name,
				ToolCallID: toolCall.ID,
			})
		}
		
		// Immediately loop back to send tool results to the model without waiting for user input
		readUserInput = false
	}

	return nil
}

func (a *Agent) runInference(ctx context.Context, conversation []openai.ChatCompletionMessage) (openai.ChatCompletionResponse, error) {
	var tools []openai.Tool
	for _, tool := range a.tools {
		tools = append(tools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}

	req := openai.ChatCompletionRequest{
		Model:       a.model,
		Messages:    conversation,
		Temperature: 0.2, // Low temperature for more reliable coding/formatting
	}
	if len(tools) > 0 {
		req.Tools = tools
	}

	return a.client.CreateChatCompletion(ctx, req)
}

func (a *Agent) executeTool(id, name string, input json.RawMessage) string {
	var toolDef ToolDefinition
	var found bool
	for _, tool := range a.tools {
		if tool.Name == name {
			toolDef = tool
			found = true
			break
		}
	}
	if !found {
		return "error: tool not found"
	}

	fmt.Printf("\033[92m[Tool Execution]\033[0m %s(%s)\n", name, string(input))
	response, err := toolDef.Function(input)
	if err != nil {
		fmt.Printf("\033[91m[Tool Error]\033[0m %s\n", err.Error())
		return fmt.Sprintf("error: %s", err.Error())
	}
	return response
}

// ============================================================================
// TOOLS DEFINITIONS
// ============================================================================

// --- 1. Read File ---
var ReadFileDefinition = ToolDefinition{
	Name:        "read_file",
	Description: "Read the contents of a given relative file path. Use this to inspect code before editing.",
	InputSchema: GenerateSchema[ReadFileInput](),
	Function:    ReadFile,
}

type ReadFileInput struct {
	Path string `json:"path" jsonschema_description:"The relative path of the file to read."`
}

func ReadFile(input json.RawMessage) (string, error) {
	var args ReadFileInput
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	content, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// --- 2. List Files ---
var ListFilesDefinition = ToolDefinition{
	Name:        "list_files",
	Description: "List files and directories at a given path. Use this to understand the project structure.",
	InputSchema: GenerateSchema[ListFilesInput](),
	Function:    ListFiles,
}

type ListFilesInput struct {
	Path string `json:"path,omitempty" jsonschema_description:"Relative path to list files from. Defaults to '.' if empty."`
}

func ListFiles(input json.RawMessage) (string, error) {
	var args ListFilesInput
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	dir := "."
	if args.Path != "" {
		dir = args.Path
	}

	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if relPath != "." && !strings.HasPrefix(relPath, ".git") { // Ignore .git spam
			if info.IsDir() {
				files = append(files, relPath+"/")
			} else {
				files = append(files, relPath)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	result, err := json.MarshalIndent(files, "", "  ")
	if err != nil {
		return "", err
	}
	return string(result), nil
}

// --- 3. Edit File ---
var EditFileDefinition = ToolDefinition{
	Name: "edit_file",
	Description: `Make exact string replacement edits to a text file.
You must ensure 'old_str' EXACTLY matches the text in the file, including all whitespaces and indentation.
If the file doesn't exist, leave 'old_str' empty to create it.`,
	InputSchema: GenerateSchema[EditFileInput](),
	Function:    EditFile,
}

type EditFileInput struct {
	Path   string `json:"path" jsonschema_description:"The path to the file"`
	OldStr string `json:"old_str" jsonschema_description:"Exact text to search for and replace."`
	NewStr string `json:"new_str" jsonschema_description:"New text to insert."`
}

func EditFile(input json.RawMessage) (string, error) {
	var args EditFileInput
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}

	if args.Path == "" || args.OldStr == args.NewStr {
		return "", fmt.Errorf("invalid parameters: path cannot be empty, and old_str must differ from new_str")
	}

	content, err := os.ReadFile(args.Path)
	if err != nil {
		if os.IsNotExist(err) && args.OldStr == "" {
			return createNewFile(args.Path, args.NewStr)
		}
		return "", err
	}

	oldContent := string(content)
	if args.OldStr != "" && !strings.Contains(oldContent, args.OldStr) {
		return "", fmt.Errorf("old_str not found in file exactly as written. Please use read_file again to check exact indentation and whitespace")
	}

	newContent := strings.Replace(oldContent, args.OldStr, args.NewStr, -1)
	if err = os.WriteFile(args.Path, []byte(newContent), 0644); err != nil {
		return "", err
	}
	return "Successfully edited file.", nil
}

func createNewFile(filePath, content string) (string, error) {
	dir := path.Dir(filePath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create directory: %w", err)
		}
	}
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	return fmt.Sprintf("Successfully created new file: %s", filePath), nil
}

// --- 4. Execute Command (NEW) ---
var ExecuteCommandDefinition = ToolDefinition{
	Name: "execute_command",
	Description: `Run a shell command in the current directory. 
Use this to run tests, compile code, execute scripts, or check system state.
Returns the standard output and standard error combined.`,
	InputSchema: GenerateSchema[ExecuteCommandInput](),
	Function:    ExecuteCommand,
}

type ExecuteCommandInput struct {
	Command string `json:"command" jsonschema_description:"The bash command to execute (e.g., 'node script.js' or 'go test')."`
}

func ExecuteCommand(input json.RawMessage) (string, error) {
	var args ExecuteCommandInput
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}

	cmd := exec.Command("sh", "-c", args.Command)
	output, err := cmd.CombinedOutput()
	
	result := string(output)
	if err != nil {
		// Include the error but also include whatever printed to stdout/stderr before failing
		return fmt.Sprintf("Command failed with error: %v\nOutput:\n%s", err, result), nil 
	}
	
	if result == "" {
		return "Command executed successfully with no output.", nil
	}
	return result, nil
}

// ============================================================================
// HELPER FUNCTIONS
// ============================================================================

func GenerateSchema[T any]() any {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
	}
	var v T
	return reflector.Reflect(v)
}