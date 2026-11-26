package connector

import "encoding/json"

// PortalMetadata keeps the fields a portal carries. Archives are handled by
// PowerLevelOverrides on the room, so nothing here is read back by the bridge;
// the map is kept so a portal written by another version keeps its fields
// instead of losing them on the next save.
type PortalMetadata map[string]json.RawMessage
