package sourcelink

import (
	"strings"
	"testing"
)

// A passage of a prompt is found where it lives: a Markdown file, a template literal with
// what the build fills in, a run of Python strings; a phrase every tool says places nothing,
// and neither do tests or comments.
func TestFindText(t *testing.T) {
	prompt := "You are Codex, a coding agent based on GPT-5.\n\n## General\n\n" +
		"- When searching for text or files, prefer using `rg` because it is much faster.\n" +
		"- Default to ASCII when editing or creating files.\n"
	files := map[string]string{
		"core/prompt.md": "# Prompt\n\n" + prompt,
		"core/src/prompts.ts": "export function corePrompt(cwd: string) {\n  return `\n" +
			"You are an interactive CLI agent specializing in software engineering tasks.\n" +
			"Your current working directory is ${cwd}, and you must stay inside it at all times.\n" +
			"# Core Mandates\n- **Conventions:** Rigorously adhere to existing project conventions.\n`;\n}\n",
		"core/src/tools.ts": "const a = 'Use this tool to read the contents of a file.';\n" +
			"// You are an interactive CLI agent specializing in software engineering tasks.\n",
		"core/src/prompts.test.ts": "const want = `You are an interactive CLI agent specializing in software engineering tasks.`;\n",
	}
	withSource(t, "fakets", Source{Repo: "acme/ts", Tag: "v%s", Dirs: []string{"core/"}, Lang: "typescript"})
	l, _ := fakeLibrary(t, "", map[string][]byte{"acme/ts@v1.0.0": tarball(t, files)})

	refs, err := l.FindText("fakets", "1.0.0", strings.ReplaceAll(prompt, "\n", " \n  "))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Path != "core/prompt.md" || refs[0].Line != 3 ||
		refs[0].URL != "https://github.com/acme/ts/blob/v1.0.0/core/prompt.md#L3-L8" {
		t.Errorf("the Markdown prompt: %+v", refs)
	}

	// As the build sent it: its directory filled in, its lines rewrapped.
	sent := "You are an interactive CLI agent specializing in software engineering tasks. Your current working " +
		"directory is <repo>, and you must stay inside it at all times. # Core Mandates - **Conventions:** " +
		"Rigorously adhere to existing project conventions."
	refs, err = l.FindText("fakets", "1.0.0", sent)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Path != "core/src/prompts.ts" || refs[0].Line != 3 || !strings.HasSuffix(refs[0].URL, "#L3-L6") {
		t.Errorf("the template literal: %+v", refs)
	}

	if refs, _ := l.FindText("fakets", "1.0.0", "Use this tool"); len(refs) != 0 {
		t.Errorf("a phrase too short to place: %+v", refs)
	}
	if refs, _ := l.FindText("fakets", "1.0.0", "Nothing in this tree says anything like this sentence at all."); len(refs) != 0 {
		t.Errorf("a passage the tree does not hold: %+v", refs)
	}
}

// Python's concatenated strings hold a prompt a line at a time, each line a piece of it.
func TestFindTextInPythonStrings(t *testing.T) {
	files := map[string]string{
		"agent/prompt_builder.py": "DEFAULT_AGENT_IDENTITY = (\n" +
			"    \"You are Hermes Agent, an intelligent AI assistant created by Nous Research. \"\n" +
			"    \"You are helpful, knowledgeable, and direct. You assist users with a wide \"\n" +
			"    f\"range of tasks including answering questions on {platform}, writing and editing code.\"\n)\n",
	}
	withSource(t, "fakepy", Source{Repo: "acme/py", Tag: "v%s", Lang: "python"})
	l, _ := fakeLibrary(t, "", map[string][]byte{"acme/py@v1.0.0": tarball(t, files)})
	refs, err := l.FindText("fakepy", "1.0.0", "You are Hermes Agent, an intelligent AI assistant created by Nous Research. "+
		"You are helpful, knowledgeable, and direct. You assist users with a wide range of tasks including answering "+
		"questions on <platform>, writing and editing code.")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Path != "agent/prompt_builder.py" || refs[0].Line != 2 || !strings.HasSuffix(refs[0].URL, "#L2-L4") {
		t.Errorf("the Python prompt: %+v", refs)
	}
}
