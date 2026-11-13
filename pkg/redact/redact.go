// Package redact blanks field values according to the keel.v1.redact annotation on proto fields, for side records
// such as audit logs and request logs. Both kinds of annotation share one blanking path: sensitive is what must not
// leak (replaced with "***"), bulky is what should not take up space (replaced with "[omitted N bytes]").
//
// Division of labour with pkg/mask: mask does value-level partial masking (keep a phone's last four digits, an
// email's domain); this package does field-level blanking. They are not merged because mask has zero dependencies
// and works on plain strings, while this package pulls in the whole protobuf reflection runtime. If RedactRule later
// grows retention policies, this package will call mask.
//
// There is only one declaration source: the annotation on the proto field. This package does not guess by field
// name: a heuristic like "something called password is probably a password" also hits token_type and captcha_id,
// while what slips through is exactly something like provisioning_uri, whose name reveals nothing and whose content
// is the most dangerous.
package redact

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/go-keel/keel/internal/errs"
	keelpb "github.com/go-keel/keel/proto/keel/v1"
)

// Mask is the replacement text for sensitive values.
//
// It is not an empty string and the field is not deleted: then "this field was not sent" and "it was sent but
// blanked" would look identical in the record, and the latter is exactly what an audit has to answer.
const Mask = "***"

// bulkyMask is the replacement text for bulky fields.
//
// Keeping the byte count is the only behavioural difference between bulky and sensitive: a password's length is
// itself intelligence, while a base64 image's size is exactly the number debugging needs.
func bulkyMask(n int) string { return fmt.Sprintf("[omitted %d bytes]", n) }

// anyName is the full name of google.protobuf.Any.
//
// Any is a real gap in redaction: its own descriptor has only two unannotated fields, type_url/value, so statically
// it never "has sensitive fields", yet protojson unpacks it by type_url when serialising and writes out every field
// of the inner message, which is how fields marked sensitive leak. Only run time knows what is inside, so the
// detection phase always treats it as having sensitive fields (messages containing Any are therefore always copied
// once), and scrub unpacks it to see what is really there.
const anyName protoreflect.FullName = "google.protobuf.Any"

// Redact returns m with every string field marked (keel.v1.redact) replaced
// by Mask (sensitive) or by its byte count (bulky), recursing into nested
// messages, lists and maps.
//
// A message with no annotated fields is **returned as is, not copied**: on this hot path (every request log passes
// through it) that is the only thing that matters. Detection is cached per descriptor, so most messages cost one map lookup.
func Redact(m proto.Message) proto.Message {
	if m == nil {
		return m
	}
	rm := m.ProtoReflect()
	if !rm.IsValid() || !hasRedacted(rm.Descriptor()) {
		return m
	}
	c := proto.Clone(m)
	scrubMessage(c.ProtoReflect())
	return c
}

// Validate scans the global registry and rejects annotations in the wrong place. mw.Default calls it and records the
// result on the chain, so a server built from it fails startup.
//
// Why at startup and not on first use: redaction fails by leaking silently, and panicking on the first production
// request would use an incident as the check. All protoc-generated code registers into GlobalFiles at init, so
// everything is visible the moment the process starts, and one pass of a few milliseconds settles it.
func Validate() error {
	var found []error
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		found = append(found, validateMessages(fd.Messages())...)
		return true
	})
	return errors.Join(found...)
}

func validateMessages(mds protoreflect.MessageDescriptors) []error {
	var found []error
	for i := 0; i < mds.Len(); i++ {
		md := mds.Get(i)
		fields := md.Fields()
		for j := 0; j < fields.Len(); j++ {
			if fd := fields.Get(j); redactOf(fd) != nil && !maskable(fd) {
				found = append(found, errs.Rules.New("redact.invalid-annotation",
					fmt.Sprintf("field %s has a keel.v1.redact annotation but its type is %s", fd.FullName(), kindOf(fd)),
					"move the annotation to a string, repeated string or map<_, string> field, or extend pkg/redact to mask that type"))
			}
		}
		found = append(found, validateMessages(md.Messages())...)
	}
	return found
}

// maskable reports whether the field's value can be replaced with Mask. A map field's Kind is message
// (the map entry), so its value type is checked separately.
func maskable(fd protoreflect.FieldDescriptor) bool {
	if fd.IsMap() {
		return fd.MapValue().Kind() == protoreflect.StringKind
	}
	return fd.Kind() == protoreflect.StringKind
}

func kindOf(fd protoreflect.FieldDescriptor) string {
	if fd.IsMap() {
		return "map value " + fd.MapValue().Kind().String()
	}
	return fd.Kind().String()
}

// redactOf returns the annotation on a field; it returns nil when unannotated or annotated with both switches false
// ("annotated { sensitive: false }" means the same as unannotated; exemption needs no extra syntax).
func redactOf(fd protoreflect.FieldDescriptor) *keelpb.RedactRule {
	opts, ok := fd.Options().(*descriptorpb.FieldOptions)
	if !ok || !proto.HasExtension(opts, keelpb.E_Redact) {
		return nil
	}
	rule, _ := proto.GetExtension(opts, keelpb.E_Redact).(*keelpb.RedactRule)
	if !rule.GetSensitive() && !rule.GetBulky() {
		return nil
	}
	return rule
}

// scrub blanks in place. It iterates in Fields() order rather than with Range: modifying the iterated object during
// Range is undefined behaviour, and map/list elements are exactly what must be changed in place.
func scrub(m protoreflect.Message) {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !m.Has(fd) {
			continue
		}
		if r := redactOf(fd); r != nil {
			if maskable(fd) { // non-strings are rejected by Validate at startup; this just avoids a panic
				maskField(m, fd, r.GetSensitive()) // both set: sensitive wins, secrecy beats saving space
			}
			continue
		}
		// Annotations may be hidden in nested messages: the annotation is on the field, and containers need not and should not be annotated.
		switch {
		case fd.IsMap():
			if isMessage(fd.MapValue().Kind()) {
				m.Mutable(fd).Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
					scrubMessage(v.Message())
					return true
				})
			}
		case fd.IsList():
			if isMessage(fd.Kind()) {
				l := m.Mutable(fd).List()
				for j := 0; j < l.Len(); j++ {
					scrubMessage(l.Get(j).Message())
				}
			}
		case isMessage(fd.Kind()):
			scrubMessage(m.Mutable(fd).Message())
		}
	}
}

// isMessage reports whether a field of this kind holds a message: GroupKind is a message with delimited encoding
// (proto2 groups, editions' message_encoding = DELIMITED) and must be searched like any other.
func isMessage(k protoreflect.Kind) bool {
	return k == protoreflect.MessageKind || k == protoreflect.GroupKind
}

// scrubMessage dispatches: Any is unpacked first, everything else recurses directly.
//
// **Every entry into the message tree must go through it, never call scrub directly**: the top-level entry, nested
// fields and the unpacked inner Any all count. Miss one and the Any there is an unlocked back door.
func scrubMessage(sub protoreflect.Message) {
	if sub.Descriptor().FullName() == anyName {
		scrubAny(sub)
		return
	}
	scrub(sub)
}

// scrubAny unpacks an Any, blanks the sensitive fields inside and packs it back into value.
//
// If type_url cannot be resolved it is let through as is: protojson cannot expand that Any either (it resolves with
// the same GlobalTypes), so serialisation fails and the content never reaches the log.
func scrubAny(m protoreflect.Message) {
	fds := m.Descriptor().Fields()
	urlFD, valFD := fds.ByName("type_url"), fds.ByName("value")
	if urlFD == nil || valFD == nil {
		return
	}
	url := m.Get(urlFD).String()
	raw := m.Get(valFD).Bytes()
	if url == "" || len(raw) == 0 {
		return
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(url[strings.LastIndex(url, "/")+1:]))
	if err != nil || !hasRedacted(mt.Descriptor()) {
		return
	}
	inner := mt.New()
	if err := proto.Unmarshal(raw, inner.Interface()); err != nil {
		return
	}
	scrubMessage(inner) // the inner message may itself be an Any
	b, err := proto.Marshal(inner.Interface())
	if err != nil {
		return // if it cannot be packed back, keep it unchanged: better unredacted than writing the Any as truncated bytes
	}
	m.Set(valFD, protoreflect.ValueOfBytes(b))
}

// maskField replaces element by element rather than clearing the container: it keeps the fact "3 tokens were sent"
// without leaking the content. Under bulky each element uses its own length, so the container's total size still adds up.
func maskField(m protoreflect.Message, fd protoreflect.FieldDescriptor, sensitive bool) {
	mask := func(old protoreflect.Value) protoreflect.Value {
		if sensitive {
			return protoreflect.ValueOfString(Mask)
		}
		return protoreflect.ValueOfString(bulkyMask(len(old.String())))
	}
	switch {
	case fd.IsMap():
		mp := m.Mutable(fd).Map()
		keys := make([]protoreflect.MapKey, 0, mp.Len())
		mp.Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
			keys = append(keys, k)
			return true
		})
		for _, k := range keys {
			mp.Set(k, mask(mp.Get(k)))
		}
	case fd.IsList():
		l := m.Mutable(fd).List()
		for i := 0; i < l.Len(); i++ {
			l.Set(i, mask(l.Get(i)))
		}
	default:
		m.Set(fd, mask(m.Get(fd)))
	}
}

// hasRedacted reports whether any field in the whole message tree is annotated, cached per descriptor.
var cache sync.Map // protoreflect.FullName -> bool

func hasRedacted(md protoreflect.MessageDescriptor) bool {
	if v, ok := cache.Load(md.FullName()); ok {
		return v.(bool)
	}
	r := treeHasRedacted(md, map[protoreflect.FullName]bool{})
	cache.Store(md.FullName(), r)
	return r
}

// treeHasRedacted caches only the result for the top-level entry: intermediate results are computed under seen's
// cut-off and may not hold from another entry. seen guards against infinite loops from self-referencing messages
// (the parent field of a tree structure).
func treeHasRedacted(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) bool {
	if md.FullName() == anyName {
		return true // the content is known only at run time; go into scrub to be safe
	}
	if seen[md.FullName()] {
		return false
	}
	seen[md.FullName()] = true
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if redactOf(fd) != nil {
			return true
		}
		if sub := subMessage(fd); sub != nil && treeHasRedacted(sub, seen) {
			return true
		}
	}
	return false
}

func subMessage(fd protoreflect.FieldDescriptor) protoreflect.MessageDescriptor {
	if fd.IsMap() {
		if isMessage(fd.MapValue().Kind()) {
			return fd.MapValue().Message()
		}
		return nil
	}
	if isMessage(fd.Kind()) {
		return fd.Message()
	}
	return nil
}
