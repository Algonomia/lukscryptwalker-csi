package driver

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// stripSecrets returns a copy of msg with every field annotated csi_secret
// cleared, so CSI requests can be logged without leaking credentials.
func stripSecrets(msg proto.Message) proto.Message {
	c := proto.Clone(msg)
	if c != nil {
		clearSecrets(c.ProtoReflect())
	}
	return c
}

func clearSecrets(m protoreflect.Message) {
	var secret []protoreflect.FieldDescriptor
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if isSecret, _ := proto.GetExtension(fd.Options(), csi.E_CsiSecret).(bool); isSecret {
			secret = append(secret, fd)
			return true
		}
		switch {
		case fd.IsList() && fd.Message() != nil:
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				clearSecrets(l.Get(i).Message())
			}
		case fd.IsMap() && fd.MapValue().Message() != nil:
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				clearSecrets(mv.Message())
				return true
			})
		case !fd.IsMap() && fd.Message() != nil:
			clearSecrets(m.Mutable(fd).Message())
		}
		return true
	})
	for _, fd := range secret {
		m.Clear(fd)
	}
}
