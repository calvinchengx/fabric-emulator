package api

import (
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Every documented ItemType must be reachable at a typed collection. A type
// with no segment 404s at the URL Microsoft's reference prints while the
// generic surface happily creates it, which looks like a caller typo.
func TestEveryItemTypeHasATypedCollection(t *testing.T) {
	aliased := map[string]bool{}
	for _, itemType := range typedCollections {
		aliased[itemType] = true
	}
	for _, itemType := range store.ItemTypes() {
		if !aliased[itemType] {
			t.Errorf("item type %s has no typed collection in typedCollections", itemType)
		}
	}
	for segment, itemType := range typedCollections {
		if _, ok := store.CanonicalItemType(itemType); !ok {
			t.Errorf("typedCollections[%q] = %q is not a documented item type", segment, itemType)
		}
	}
}
