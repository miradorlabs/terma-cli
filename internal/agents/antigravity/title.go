package antigravity

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// agy names a conversation in the background once it begins, with a model call of its own,
// and again when the developer renames it. No hook's payload carries the name, and the status
// line's only in an interactive agy. agy keeps it in <appDataDir>/annotations/<id>.pbtxt, a
// ConversationAnnotations in protobuf text format beside the brain folder that holds the
// transcript. Each turn's start and its Stop read it and spool a new or changed name as
// terma.session.title. The name restates the first prompt, so it goes only where the hook may
// send prompts (contentFor), and at delivery under the policy's prompt switch.

// sourceAntigravityAnnotations is terma.evidence.source for the name agy's annotations hold.
const sourceAntigravityAnnotations = "antigravity_annotations"

// antigravityTitleDir holds a hash of the name each conversation last spooled.
var antigravityTitleDir = hookrun.AgentStateDir(Agent{}.Name(), "titles")

// antigravityAnnotationsMaxBytes bounds the read: a conversation's annotations are a name,
// a few flags and tags.
const antigravityAnnotationsMaxBytes = 64 << 10

// antigravityAnnotations is the path of the conversation's annotations, beside the brain
// folder its transcript is in; "" without a transcript of agy's shape.
func antigravityAnnotations(in *antigravityHookInput) string {
	transcript := antigravityTranscript(in)
	if transcript == "" {
		return ""
	}
	// <appDataDir>/brain/<conversationId>/.system_generated/logs/transcript_full.jsonl
	appData := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(transcript)))))
	return filepath.Join(appData, "annotations", in.id()+".pbtxt")
}

// annotationsTitle is the one field terma reads of agy's ConversationAnnotations, as a
// message of its own: the text format names fields, so the number is not read, and every
// other field is passed over.
var annotationsTitle = sync.OnceValues(func() (protoreflect.MessageDescriptor, error) {
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("terma/antigravity/annotations.proto"),
		Package: proto.String("terma.antigravity"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("ConversationAnnotations"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("title"),
				JsonName: proto.String("title"),
				Number:   proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			}},
		}},
	}, nil)
	if err != nil {
		return nil, err
	}
	return file.Messages().Get(0), nil
})

// readAntigravityTitle is the name the conversation's annotations at path hold, trimmed;
// found is false before agy has named it. A file that is not a regular one, too long or not
// in the text format is an error.
func readAntigravityTitle(path string) (title string, found bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|hookrun.EvidenceOpenFlags, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, errors.New("annotations are not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, antigravityAnnotationsMaxBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > antigravityAnnotationsMaxBytes {
		return "", false, errors.New("annotations are too long")
	}
	desc, err := annotationsTitle()
	if err != nil {
		return "", false, err
	}
	msg := dynamicpb.NewMessage(desc)
	if err := (prototext.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, msg); err != nil {
		return "", false, err
	}
	title = strings.TrimSpace(msg.Get(desc.Fields().Get(0)).String())
	return title, title != "", nil
}

// spoolTitle spools the conversation's name when it is new or changed, while the hook holds
// the conversation, which it does only with a transcript of agy's shape, and may send its
// prompts.
func (c *antigravityConversation) spoolTitle() {
	if !c.held || !c.contentFor().prompts {
		return
	}
	path := antigravityAnnotations(c.in)
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaEvidenceSourceKey: sourceAntigravityAnnotations}
	hookrun.SpoolTitle(c.at(c.e), c.r, antigravityTitleDir, c.in.id(), attrs, func() (string, bool, error) {
		return readAntigravityTitle(path)
	})
}
