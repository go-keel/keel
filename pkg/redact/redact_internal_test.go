// In-package: calls validateMessages on hand-built descriptors; the public API validates only linked messages.

package redact

import (
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	keelpb "github.com/go-keel/keel/proto/keel/v1"
)

// The descriptors for the tests are built in code rather than in a .proto file: this repo does not create protos for
// demonstration, and these shapes (sensitive annotations on string / repeated / nested / int) do not belong to any
// real contract.

func str(name string, num int32, opts *descriptorpb.FieldOptions) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name: proto.String(name), Number: proto.Int32(num),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		JsonName: proto.String(name), Options: opts,
	}
}

func redacted() *descriptorpb.FieldOptions {
	o := &descriptorpb.FieldOptions{}
	proto.SetExtension(o, keelpb.E_Redact, &keelpb.RedactRule{Sensitive: true})
	return o
}

func bulk() *descriptorpb.FieldOptions {
	o := &descriptorpb.FieldOptions{}
	proto.SetExtension(o, keelpb.E_Redact, &keelpb.RedactRule{Bulky: true})
	return o
}

func both() *descriptorpb.FieldOptions {
	o := &descriptorpb.FieldOptions{}
	proto.SetExtension(o, keelpb.E_Redact, &keelpb.RedactRule{Sensitive: true, Bulky: true})
	return o
}

// buildFile assembles: Plain{username} / Secret{username, password*, tokens*[]} /
// Outer{note, inner Secret} / Photo{caption, image+, scan+*}, where * is sensitive and
// + is bulky.
func buildFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	tokens := str("tokens", 3, redacted())
	tokens.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()

	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("keel/redacttest/t.proto"),
		Package: proto.String("keel.redacttest"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Plain"), Field: []*descriptorpb.FieldDescriptorProto{str("username", 1, nil)}},
			{Name: proto.String("Secret"), Field: []*descriptorpb.FieldDescriptorProto{
				str("username", 1, nil), str("password", 2, redacted()), tokens,
			}},
			{Name: proto.String("Photo"), Field: []*descriptorpb.FieldDescriptorProto{
				str("caption", 1, nil), str("image", 2, bulk()), str("scan", 3, both()),
			}},
			{Name: proto.String("Outer"), Field: []*descriptorpb.FieldDescriptorProto{
				str("note", 1, nil),
				{
					Name: proto.String("inner"), Number: proto.Int32(2),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					TypeName: proto.String(".keel.redacttest.Secret"),
					JsonName: proto.String("inner"),
				},
			}},
		},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd
}

func msg(t *testing.T, fd protoreflect.FileDescriptor, name string) *dynamicpb.Message {
	t.Helper()
	md := fd.Messages().ByName(protoreflect.Name(name))
	if md == nil {
		t.Fatalf("message %s not found", name)
	}
	return dynamicpb.NewMessage(md)
}

func set(m *dynamicpb.Message, field, v string) {
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(field))
	m.Set(fd, protoreflect.ValueOfString(v))
}

func get(t *testing.T, m proto.Message, path ...string) string {
	t.Helper()
	rm := m.ProtoReflect()
	for i, p := range path {
		fd := rm.Descriptor().Fields().ByName(protoreflect.Name(p))
		if i == len(path)-1 {
			return rm.Get(fd).String()
		}
		rm = rm.Get(fd).Message()
	}
	return ""
}

func TestRedactMasksSensitiveKeepsRest(t *testing.T) {
	fd := buildFile(t)
	m := msg(t, fd, "Secret")
	set(m, "username", "alice")
	set(m, "password", "sup3r-s3cret")

	out := Redact(m)
	if got := get(t, out, "password"); got != Mask {
		t.Errorf("password = %q, want %q", got, Mask)
	}
	// Redaction must not affect plain fields, otherwise the audit records nothing useful
	if got := get(t, out, "username"); got != "alice" {
		t.Errorf("username = %q, want alice", got)
	}
	// The original message must not change: the middleware hands it to the business chain, so the copy is what gets blanked
	if got := get(t, m, "password"); got != "sup3r-s3cret" {
		t.Errorf("original message modified in place: %q", got)
	}
}

// With no sensitive fields it returns as is, without copying: every request log passes through it, the only performance point that matters.
// bulky keeps the byte count and sensitive does not, the only behavioural difference; when both are set sensitive wins.
func TestRedactBulkyKeepsSize(t *testing.T) {
	fd := buildFile(t)
	m := msg(t, fd, "Photo")
	set(m, "caption", "avatar")
	set(m, "image", strings.Repeat("A", 1234))
	set(m, "scan", strings.Repeat("B", 99))

	out := Redact(m)
	if got := get(t, out, "image"); got != "[omitted 1234 bytes]" {
		t.Errorf("image = %q", got)
	}
	if got := get(t, out, "scan"); got != Mask {
		t.Errorf("scan = %q, with both sensitive+bulky it should be treated as sensitive (the length is intelligence)", got)
	}
	if got := get(t, out, "caption"); got != "avatar" {
		t.Errorf("caption = %q, an unannotated field must not be touched", got)
	}
	if got := get(t, m, "image"); got != strings.Repeat("A", 1234) {
		t.Error("original message modified in place")
	}
}

func TestRedactNoSensitiveIsZeroCopy(t *testing.T) {
	fd := buildFile(t)
	m := msg(t, fd, "Plain")
	set(m, "username", "alice")
	if out := Redact(m); out != proto.Message(m) {
		t.Error("a message without sensitive fields was copied")
	}
}

func TestRedactRepeatedKeepsLength(t *testing.T) {
	fd := buildFile(t)
	m := msg(t, fd, "Secret")
	l := m.Mutable(m.Descriptor().Fields().ByName("tokens")).List()
	l.Append(protoreflect.ValueOfString("t1"))
	l.Append(protoreflect.ValueOfString("t2"))

	out := Redact(m).ProtoReflect()
	got := out.Get(out.Descriptor().Fields().ByName("tokens")).List()
	// Keep the fact "2 were sent", blank only the content
	if got.Len() != 2 {
		t.Fatalf("tokens length = %d, want 2 (the list must not be cleared)", got.Len())
	}
	for i := 0; i < got.Len(); i++ {
		if got.Get(i).String() != Mask {
			t.Errorf("tokens[%d] = %q, want %q", i, got.Get(i).String(), Mask)
		}
	}
}

// The annotation is on the field and containers need not and should not be annotated: sensitive fields in nested messages must be found.
func TestRedactNested(t *testing.T) {
	fd := buildFile(t)
	outer := msg(t, fd, "Outer")
	set(outer, "note", "n")
	inner := outer.Mutable(outer.Descriptor().Fields().ByName("inner")).Message()
	inner.Set(inner.Descriptor().Fields().ByName("password"), protoreflect.ValueOfString("p"))

	if got := get(t, Redact(outer), "inner", "password"); got != Mask {
		t.Errorf("nested password = %q, want %q", got, Mask)
	}
}

// A message field with delimited encoding (editions' message_encoding = DELIMITED, Kind GroupKind) is searched like
// any other message field.
func TestRedactDelimitedField(t *testing.T) {
	opts := &descriptorpb.FieldOptions{Features: &descriptorpb.FeatureSet{
		MessageEncoding: descriptorpb.FeatureSet_DELIMITED.Enum(),
	}}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("keel/redacttest/delimited.proto"),
		Package: proto.String("keel.redacttest.delimited"),
		Syntax:  proto.String("editions"),
		Edition: descriptorpb.Edition_EDITION_2023.Enum(),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Secret"), Field: []*descriptorpb.FieldDescriptorProto{str("password", 1, redacted())}},
			{Name: proto.String("Outer"), Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("inner"), Number: proto.Int32(1),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				TypeName: proto.String(".keel.redacttest.delimited.Secret"),
				JsonName: proto.String("inner"),
				Options:  opts,
			}}},
		},
	}, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	outer := msg(t, fd, "Outer")
	innerFD := outer.Descriptor().Fields().ByName("inner")
	if innerFD.Kind() != protoreflect.GroupKind {
		t.Fatalf("setup: inner kind = %v, want group", innerFD.Kind())
	}
	inner := outer.Mutable(innerFD).Message()
	inner.Set(inner.Descriptor().Fields().ByName("password"), protoreflect.ValueOfString("p"))

	if got := get(t, Redact(outer), "inner", "password"); got != Mask {
		t.Errorf("delimited inner password = %q, want %q", got, Mask)
	}
}

// A wrongly typed annotation must error at startup rather than silently not blank at run time.
func TestValidateRejectsNonString(t *testing.T) {
	bad := &descriptorpb.FieldDescriptorProto{
		Name: proto.String("retries"), Number: proto.Int32(1),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		JsonName: proto.String("retries"), Options: redacted(),
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("keel/redacttest/bad.proto"), Package: proto.String("keel.redacttestbad"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("B"), Field: []*descriptorpb.FieldDescriptorProto{bad}}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	if errs := validateMessages(fd.Messages()); len(errs) == 0 {
		t.Error("sensitive on int32 was not rejected")
	}
}

// secretType is a test sensitive message (username + password*) registered in GlobalTypes.
//
// It must be in the global registry: unpacking Any goes through GlobalTypes for both redact and protojson. The global
// registry has no unregister API and registering twice panics, so it is wrapped in a Once for `go test -count=N` to work.
var secretType = sync.OnceValues(func() (protoreflect.MessageType, error) {
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("keel/redactanytest/inner.proto"),
		Package: proto.String("keel.redactanytest"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Secret"),
			Field: []*descriptorpb.FieldDescriptorProto{
				str("username", 1, nil), str("password", 2, redacted()),
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		return nil, err
	}
	mt := dynamicpb.NewMessageType(fd.Messages().Get(0))
	if err := protoregistry.GlobalTypes.RegisterMessage(mt); err != nil {
		return nil, err
	}
	return mt, nil
})

// packedSecret returns an Any holding {username:"alice", password:"leaked-secret"}.
func packedSecret(t *testing.T) *anypb.Any {
	t.Helper()
	mt, err := secretType()
	if err != nil {
		t.Fatalf("register test type: %v", err)
	}
	m := mt.New()
	fds := m.Descriptor().Fields()
	m.Set(fds.ByName("username"), protoreflect.ValueOfString("alice"))
	m.Set(fds.ByName("password"), protoreflect.ValueOfString("leaked-secret"))
	raw, err := proto.Marshal(m.Interface())
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	return &anypb.Any{TypeUrl: "type.googleapis.com/keel.redactanytest.Secret", Value: raw}
}

// TestRedactAny: Any is the escape hatch of redaction. Its own descriptor has only two unannotated fields,
// type_url/value, so statically it is always "clean", yet protojson unpacks it by type_url and expands every inner
// field. All three positions must be sealed: passed in at top level, embedded as a field in another message, and Any in Any.
//
// The assertions take the real path (protojson serialisation), because the line between leaking and not is exactly that expansion.
func TestRedactAny(t *testing.T) {
	envelope := func(t *testing.T, a *anypb.Any) proto.Message {
		t.Helper()
		fdp := &descriptorpb.FileDescriptorProto{
			Name:       proto.String("keel/redactanytest/outer.proto"),
			Package:    proto.String("keel.redactanytest"),
			Syntax:     proto.String("proto3"),
			Dependency: []string{"google/protobuf/any.proto"},
			MessageType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Envelope"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("payload"), Number: proto.Int32(1),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					TypeName: proto.String(".google.protobuf.Any"),
					JsonName: proto.String("payload"),
				}},
			}},
		}
		fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
		if err != nil {
			t.Fatalf("outer NewFile: %v", err)
		}
		env := dynamicpb.NewMessage(fd.Messages().Get(0))
		p := env.Mutable(env.Descriptor().Fields().ByName("payload")).Message()
		pf := p.Descriptor().Fields()
		p.Set(pf.ByName("type_url"), protoreflect.ValueOfString(a.GetTypeUrl()))
		p.Set(pf.ByName("value"), protoreflect.ValueOfBytes(a.GetValue()))
		return env
	}

	nestedAny := func(t *testing.T, a *anypb.Any) proto.Message {
		t.Helper()
		b, err := proto.Marshal(a)
		if err != nil {
			t.Fatalf("marshal inner any: %v", err)
		}
		return &anypb.Any{TypeUrl: "type.googleapis.com/google.protobuf.Any", Value: b}
	}

	cases := []struct {
		name string
		of   func(*testing.T, *anypb.Any) proto.Message
	}{
		{"top level is Any", func(_ *testing.T, a *anypb.Any) proto.Message { return a }},
		{"Any as a field", envelope},
		{"Any in Any", nestedAny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := packedSecret(t)
			j, err := protojson.Marshal(Redact(c.of(t, a)))
			if err != nil {
				t.Fatalf("protojson: %v", err)
			}
			if strings.Contains(string(j), "leaked-secret") {
				t.Fatalf("plaintext password in Any leaked into serialised output: %s", j)
			}
			if !strings.Contains(string(j), "alice") {
				t.Errorf("plain field in Any lost (must not be affected): %s", j)
			}
			// The original message must not change in place: the middleware hands it to the business chain, so the copy is what gets blanked
			if !strings.Contains(string(a.GetValue()), "leaked-secret") {
				t.Error("original Any's value modified in place")
			}
		})
	}
}
