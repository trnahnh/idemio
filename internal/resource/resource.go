package resource

type Definition struct {
	Type       string
	Operations map[string]string
}

var registry = map[string]Definition{
	"invoice": {
		Type: "invoice",
		Operations: map[string]string{
			"create_charge": "create",
		},
	},
}

func Lookup(resourceType string) (Definition, bool) {
	definition, ok := registry[resourceType]
	return definition, ok
}

func ClassOf(resourceType, operation string) (string, bool) {
	definition, ok := registry[resourceType]
	if !ok {
		return "", false
	}
	class, ok := definition.Operations[operation]
	return class, ok
}
