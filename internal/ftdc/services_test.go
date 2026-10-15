package ftdc

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

// An 8.0 shard nests its sample under common/ and shard/; the paths come out as every other
// version spells them, and a name both sections carry keeps its prefix rather than merging.
func TestEightZeroServiceSectionsAreUnwrapped(t *testing.T) {
	raw, err := bson.Marshal(bson.D{
		{Key: "start", Value: int64(1)},
		{Key: "common", Value: bson.D{
			{Key: "start", Value: int64(2)},
			{Key: "serverStatus", Value: bson.D{{Key: "version", Value: "8.0.0"}, {Key: "uptime", Value: int64(10)}}},
		}},
		{Key: "shard", Value: bson.D{{Key: "replSetGetStatus", Value: bson.D{{Key: "ok", Value: int64(1)}}}}},
		{Key: "router", Value: bson.D{{Key: "serverStatus", Value: bson.D{{Key: "uptime", Value: int64(10)}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err := columns(raw)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, c := range found {
		paths[c.path] = true
	}
	for _, want := range []string{"start", "common/start", "serverStatus/uptime", "replSetGetStatus/ok", "router/serverStatus/uptime"} {
		if !paths[want] {
			t.Errorf("missing %s among %v", want, paths)
		}
	}
	if v, err := lookup(raw, "serverStatus", "version"); err != nil || v.StringValue() != "8.0.0" {
		t.Errorf("the version under common/ was not found: %v %v", v, err)
	}
}
