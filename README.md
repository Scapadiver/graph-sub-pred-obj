# graph-sub-pred-obj

A high-performance RDF graph store in Go using Aerospike 8.2.0.0 as the key-value backend. Stores Subject-Predicate-Object triples with optional metadata properties and supports all standard RDF query patterns plus multi-hop graph traversal.

## Requirements

- Go 1.25+
- Aerospike Server 8.2.0.0+
- Python 3.12+ (for the graph viewer)

## Usage

```bash
go build -o graph-spo .
./graph-spo -host 127.0.0.1 -port 3000 -namespace test
```

| Flag | Default | Description |
|------|---------|-------------|
| `-host` | `127.0.0.1` | Aerospike server host |
| `-port` | `3000` | Aerospike server port |
| `-namespace` | `test` | Aerospike namespace |

## Data Model

Each triple is stored as an Aerospike record in the `triples` set:

| Bin | Type | Description |
|-----|------|-------------|
| `subject` | String | Subject URI/identifier |
| `predicate` | String | Predicate/relationship type |
| `object` | String | Object URI/literal |
| `props` | CDT Map | Optional metadata (timestamps, weights, labels, etc.) |

The primary key is a SHA-256 hash of `subject + \0 + predicate + \0 + object`, providing natural deduplication and even partition distribution.

## Query Patterns

All 7 RDF query patterns are supported, automatically selecting the optimal index strategy:

| Pattern | Example | Method |
|---------|---------|--------|
| `SPO` | Does Alice follow Bob? | Direct PK lookup (sub-ms) |
| `SP?` | Who does Bob follow? | SI on subject + filter on predicate |
| `S?O` | How is Alice connected to post:101? | SI on subject + filter on object |
| `S??` | Everything about Alice | SI on subject |
| `?PO` | Who authored post:101? | SI on predicate + filter on object |
| `?P?` | All "authored" relationships | SI on predicate |
| `??O` | Everything pointing to topic:graphs | SI on object |

Three secondary indexes are created automatically: `idx_subject`, `idx_predicate`, `idx_object`.

## Content Generator

A standalone tool that reads JSON descriptor files and generates synthetic identity graph data at scale. Driven by three configuration files:

- **`descriptors/contentTypes.json`** — defines 6 entity types (INDIVIDUAL, ACCOUNT, DISPLAY_DEVICE, ADDRESS, CREDIT_DEVICE, HOUSEHOLD), each with their own set of identifier properties and decay rates
- **`descriptors/ontology.json`** — defines relationship rules between entity types (e.g., INDIVIDUAL → HAS_ONE_OR_MORE → ACCOUNT) with cardinality semantics
- **`descriptors/cardinalities.json`** — caps how many INDIVIDUALs may share one entity (e.g., an ACCOUNT is shared by at most 4 INDIVIDUALs, an ADDRESS by at most 2)

### Running the Generator

```bash
go run ./cmd/generate/ \
  -host 127.0.0.1 \
  -port 3000 \
  -namespace test \
  -count 1000000 \
  -batch 256 \
  -workers 8
```

| Flag | Default | Description |
|------|---------|-------------|
| `-count` | `1000` | Number of INDIVIDUALs to generate |
| `-batch` | `256` | Batch write size |
| `-workers` | `8` | Concurrent writer goroutines |
| `-content-types` | `descriptors/contentTypes.json` | Path to content types descriptor |
| `-ontology` | `descriptors/ontology.json` | Path to ontology descriptor |
| `-cardinalities` | `descriptors/cardinalities.json` | Path to cardinalities descriptor |
| `-share-rate` | `0.2` | Probability an INDIVIDUAL links to an existing entity instead of creating a new one |
| `-host` | `127.0.0.1` | Aerospike host |
| `-port` | `3000` | Aerospike port |
| `-namespace` | `test` | Aerospike namespace |

### What It Generates

Each INDIVIDUAL produces ~80 triples including:
- **IS_TYPE** triple linking entity to its content type
- **HAS_IDENTIFIER** triples for each property defined in contentTypes.json, with `created`, `last_seen`, and `decay` metadata
- **Relationship triples** following ontology rules (HAS_ONE_OR_MORE, LIVES_AT, IS_PART_OF, etc.)
- **Shared entities**: ~20% of INDIVIDUAL links to an ACCOUNT, ADDRESS, CREDIT_DEVICE, or DISPLAY_DEVICE reuse an existing entity (up to its cardinality cap), and ~20% of INDIVIDUALs join an existing HOUSEHOLD

### Sharing and Incremental Runs

Sharing is expressed purely as triples that reuse an existing entity's ID, so relations form naturally when the RDF content is loaded. A shared entity is linked to, but its own identifier, type, and outgoing relationship triples are not generated again.

Before generating, the tool scans the existing `triples` set once to:
- **Resume ID numbering** above the highest ID already stored, so repeated runs never collide with earlier data
- **Seed the share pools** with existing entities and how many INDIVIDUALs already link to each, so new content links into the existing graph without exceeding the caps

Entities that reach their cap leave the pool. Each pool holds up to 100,000 entities per type. Two generator runs writing at the same time can still collide, so run them one at a time.

#### cardinalities.json

```json
{
	"cardinalities":
		[
			{"TYPE": "ACCOUNT",        "LINKED_TO": "INDIVIDUAL", "MAX": 4},
			{"TYPE": "ADDRESS",        "LINKED_TO": "INDIVIDUAL", "MAX": 2},
			{"TYPE": "CREDIT_DEVICE",  "LINKED_TO": "INDIVIDUAL", "MAX": 4},
			{"TYPE": "DISPLAY_DEVICE", "LINKED_TO": "INDIVIDUAL", "MAX": 6}
		]
}
```

Types without an entry are never shared. Only `LINKED_TO: "INDIVIDUAL"` is supported; other entries are ignored with a warning.

### Performance

Benchmarked at ~300K triples/sec with 8 workers on a single Aerospike node.

## Interactive Graph Viewer

A web-based property graph viewer built with Python, Dash, and Cytoscape. Connects directly to Aerospike and provides interactive visualization of RDF relationships.

### Setup

```bash
pip install -r viewer/requirements.txt
python viewer/app.py --host 127.0.0.1 --port 3000 --namespace test
```

Then open http://127.0.0.1:8050 in your browser.

### Features

- **Start Node** search: type 2+ characters to find a node (up to 50 matches are shown, so large graphs don't overload the browser)
- **Max Hops** slider (1-6) to control traversal depth
- **Predicate Filter** to show only specific relationship types
- **Direction** control for outbound, inbound, or bidirectional traversal
- Changing Max Hops, Predicate Filter, or Direction rebuilds the current graph immediately
- **Multiple layouts**: cola (force-directed), dagre (hierarchical), breadthfirst, circle, concentric, grid
- **Click** a node or edge to inspect all properties in the info panel (up to 50 edges listed per direction)
- **Click** an unexpanded node to expand its relationships inline
- **Graph stats** panel showing node/edge counts, predicates, type breakdown, and hubs
- **Color-coded nodes** by type prefix (blue=user, green=post, orange=topic)

### Hub Limit

Nodes with more than 10 edges are treated as **hubs** and are not expanded, so a shared node doesn't pull thousands of neighbors into the graph. For example, every entity links to its bare type node (`INDIVIDUAL`, `ACCOUNT`, ...) through `IS_TYPE`.

- Hubs still appear in the graph, labeled with their edge count (e.g. `INDIVIDUAL (12038 edges)`) and drawn with a dashed orange border
- Graph Stats lists the hubs that were not expanded, largest first
- Clicking a hub does not expand it
- The start node is always expanded, whatever its degree
- Degree is counted after the predicate filter and direction are applied, so a node can be a hub under one filter and expandable under another

Change the threshold with `MAX_EXPAND_DEGREE` at the top of `viewer/app.py`.

### Viewer CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--host` | `127.0.0.1` | Aerospike host |
| `--port` | `3000` | Aerospike port |
| `--namespace` | `test` | Aerospike namespace |
| `--debug` | off | Enable Dash debug mode with hot reload |

## Package Structure

```
model/triple.go      Triple struct, key hashing, type helpers
store/client.go      Aerospike connection and policy configuration
store/index.go       Secondary index creation
store/writer.go      PutTriple, BatchPutTriples, DeleteTriple, UpdateTripleProperty
store/reader.go      GetTriple, QueryBySubject/Predicate/Object, QuerySP/PO/SO, ScanAll
graph/query.go       PatternQuery dispatcher for all 7 RDF patterns
graph/traverse.go    BFS outbound/inbound traversal with depth limits
ingest/loader.go     Batch and channel-based streaming ingestion
cmd/generate/main.go Content generator driven by descriptor JSON files
viewer/app.py        Interactive web-based graph viewer (Dash + Cytoscape)
descriptors/         Content type and ontology JSON descriptors
```

## Example

```go
gs, _ := store.NewGraphStore("127.0.0.1", 3000, "test")
defer gs.Close()
gs.CreateIndexes()

// Write a triple with properties
gs.PutTriple(&model.Triple{
    Subject:   "user:alice",
    Predicate: "follows",
    Object:    "user:bob",
    Props:     map[string]interface{}{"since": time.Now().UnixMilli()},
})

// Query: who does alice follow?
results, _ := gs.QuerySP("user:alice", "follows")

// Traverse: BFS from alice, 3 hops deep, follows-only
paths, _ := graph.TraverseOutbound(gs, "user:alice", 3, "follows")

// Pattern query: find how alice connects to post:101
pq := &graph.PatternQuery{Subject: graph.S("user:alice"), Object: graph.O("post:101")}
results, _ = pq.Execute(gs)
```

## Ingestion

Batch loader for bulk writes (default 256 records/batch):

```go
loader := ingest.NewLoader(gs, 256)
loader.LoadTriples(triples)
```

Channel-based streaming for continuous ingestion:

```go
ch := make(chan *model.Triple)
go loader.LoadFromChannel(ch)
ch <- &model.Triple{Subject: "a", Predicate: "b", Object: "c"}
close(ch)
```

## Performance

- **SPO lookups**: Sub-millisecond via direct primary key get
- **Batch writes**: Single network round-trip for up to 256 records
- **Property updates**: Atomic CDT map operations (no read-modify-write)
- **Server-side filtering**: Compound queries filter on the server, not the client
- **Concurrent traversal**: Parallel SI queries per BFS level with configurable concurrency
