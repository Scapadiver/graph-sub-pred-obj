package main

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"graph-sub-pred-obj/model"
	"graph-sub-pred-obj/store"
)

// ---------------------------------------------------------------------------
// Descriptor types
// ---------------------------------------------------------------------------

type PropertyMeta struct {
	Created  int64   `json:"CREATED"`
	LastSeen int64   `json:"LAST_SEEN"`
	Decay    float64 `json:"DECAY"`
}

type ContentTypesFile struct {
	ContentTypes map[string]map[string][]PropertyMeta `json:"contentTypes"`
}

type OntologyRelation struct {
	Sub  string `json:"SUB"`
	Pred string `json:"PRED"`
	Obj  string `json:"OBJ"`
}

type OntologyFile struct {
	Ontology []OntologyRelation `json:"ontology"`
}

// Cardinality caps how many entities of LinkedTo type may link to one entity
// of Type, e.g. one ACCOUNT is shared by at most 4 INDIVIDUALs.
type Cardinality struct {
	Type     string `json:"TYPE"`
	LinkedTo string `json:"LINKED_TO"`
	Max      int    `json:"MAX"`
}

type CardinalitiesFile struct {
	Cardinalities []Cardinality `json:"cardinalities"`
}

// ---------------------------------------------------------------------------
// Cardinality rules derived from ontology predicates
// ---------------------------------------------------------------------------

type cardinalityRange struct {
	min int
	max int
}

var predicateCardinality = map[string]cardinalityRange{
	"HAS_ONE_OR_MORE":  {1, 3},
	"LIVES_AT":         {1, 1},
	"IS_PART_OF":       {1, 1},
	"HAS":              {1, 3},
	"CAN_HAVE_SEVERAL": {1, 5},
	"HAS_AN":           {1, 1},
}

// ---------------------------------------------------------------------------
// ID generators
// ---------------------------------------------------------------------------

var idCounter atomic.Int64

func nextID() int64 {
	return idCounter.Add(1)
}

// Counter values embedded in entity IDs ("ACCOUNT:123") and identifier values
// ("EMAIL_E:EMAIL_E_123_abc"), used to resume numbering above existing data.
var (
	entityIDPattern     = regexp.MustCompile(`^[A-Z_]+:(\d+)$`)
	identifierIDPattern = regexp.MustCompile(`_(\d+)_[0-9a-f]+$`)
)

func idCounterValue(id string) int64 {
	m := entityIDPattern.FindStringSubmatch(id)
	if m == nil {
		m = identifierIDPattern.FindStringSubmatch(id)
	}
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// idCounterName is the meta record holding the highest ID used, since IDs
// can't be recovered from hashed content.
const idCounterName = "id_counter"

// ---------------------------------------------------------------------------
// Content hashing
// ---------------------------------------------------------------------------

// hashContent is applied to every subject, predicate, object and string prop
// as it is created. Hashing is deterministic, so identical content still links.
var hashContent = func(s string) string { return s }

func newHashFunc(hashType string) (func(string) string, error) {
	switch hashType {
	case "no-hash":
		return func(s string) string { return s }, nil
	case "sha-256":
		return func(s string) string {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:])
		}, nil
	case "sha-512":
		return func(s string) string {
			sum := sha512.Sum512([]byte(s))
			return hex.EncodeToString(sum[:])
		}, nil
	}
	return nil, fmt.Errorf("unknown hash type %q (want no-hash, sha-256, or sha-512)", hashType)
}

func generateEntityID(contentType string) string {
	return hashContent(fmt.Sprintf("%s:%d", contentType, nextID()))
}

func generateIdentifierValue(propName string) string {
	return fmt.Sprintf("%s_%d_%x", propName, nextID(), rand.Int63()&0xFFFFFF)
}

// ---------------------------------------------------------------------------
// Entity generation
// ---------------------------------------------------------------------------

func generateEntityTriples(entityID string, contentType string, propDefs map[string][]PropertyMeta, now int64) []*model.Triple {
	var triples []*model.Triple

	for propName, metas := range propDefs {
		if len(metas) == 0 {
			continue
		}
		meta := metas[0]
		value := generateIdentifierValue(propName)

		triples = append(triples, &model.Triple{
			Subject:   entityID,
			Predicate: hashContent("HAS_IDENTIFIER"),
			Object:    hashContent(fmt.Sprintf("%s:%s", propName, value)),
			Props: map[string]interface{}{
				"identifier_type": hashContent(propName),
				"value":           hashContent(value),
				"created":         now,
				"last_seen":       now,
				"decay":           meta.Decay,
			},
		})
	}

	// Entity type triple
	triples = append(triples, &model.Triple{
		Subject:   entityID,
		Predicate: hashContent("IS_TYPE"),
		Object:    hashContent(contentType),
		Props: map[string]interface{}{
			"created": now,
		},
	})

	return triples
}

// ---------------------------------------------------------------------------
// Entity sharing
// ---------------------------------------------------------------------------

// maxPoolPerType bounds memory; once full, new entities replace random slots.
const maxPoolPerType = 100000

// sharePool holds entities that individuals may link to instead of creating a
// new one. Sharing is expressed only by triples reusing the same entity ID, so
// relations form naturally when the RDF is loaded. An entity leaves the pool
// once it reaches its cardinality cap.
type sharePool struct {
	mu     sync.Mutex
	rate   float64
	caps   map[string]int      // entity type -> max INDIVIDUALs per entity
	avail  map[string][]string // entity type -> IDs below cap
	counts map[string]int      // entity ID -> INDIVIDUALs linked
}

func newSharePool(rate float64, cards []Cardinality) *sharePool {
	p := &sharePool{
		rate:   rate,
		caps:   make(map[string]int),
		avail:  make(map[string][]string),
		counts: make(map[string]int),
	}
	for _, c := range cards {
		if c.LinkedTo != "INDIVIDUAL" {
			log.Printf("cardinality %s -> %s ignored: only INDIVIDUAL links are supported", c.Type, c.LinkedTo)
			continue
		}
		p.caps[c.Type] = c.Max
	}
	return p
}

// add offers an entity already linked to count individuals for sharing.
func (p *sharePool) add(entityType, id string, count int, rng *rand.Rand) {
	if count >= p.caps[entityType] {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.avail[entityType]
	if len(list) < maxPoolPerType {
		p.avail[entityType] = append(list, id)
	} else {
		i := rng.Intn(len(list))
		delete(p.counts, list[i])
		list[i] = id
	}
	p.counts[id] = count
}

// pick returns, with probability rate, an existing entity of entityType that
// is below its cap and not in exclude, recording the new link.
func (p *sharePool) pick(entityType string, exclude map[string]bool, rng *rand.Rand) (string, bool) {
	if p.caps[entityType] < 2 || rng.Float64() >= p.rate {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.avail[entityType]
	for attempt := 0; attempt < 3 && len(list) > 0; attempt++ {
		i := rng.Intn(len(list))
		id := list[i]
		if exclude[id] {
			continue
		}
		p.counts[id]++
		if p.counts[id] >= p.caps[entityType] {
			list[i] = list[len(list)-1]
			p.avail[entityType] = list[:len(list)-1]
			delete(p.counts, id)
		}
		return id, true
	}
	return "", false
}

// individualPredicates returns the predicates only ever used with an
// INDIVIDUAL subject, so links can be counted without knowing subject types.
func individualPredicates(ontology []OntologyRelation) map[string]bool {
	preds := make(map[string]bool)
	for _, rel := range ontology {
		if rel.Sub == "INDIVIDUAL" {
			preds[rel.Pred] = true
		}
	}
	for _, rel := range ontology {
		if rel.Sub != "INDIVIDUAL" {
			delete(preds, rel.Pred)
		}
	}
	return preds
}

// loadExisting scans existing triples once to resume ID numbering above
// existing data (starting from savedID, the stored high-water mark) and to
// seed the share pools with existing entities, so new content links into the
// graph already loaded. Entity types come from IS_TYPE triples, so this works
// on hashed content; only content hashed the same way is matched.
func loadExisting(scan func(func(*model.Triple) error) error, savedID int64, pool *sharePool, ontology []OntologyRelation, sharedHouseholds map[string]string, rng *rand.Rand) (int, error) {
	maxID := savedID

	isType := hashContent("IS_TYPE")
	household := hashContent("HOUSEHOLD")
	typeNames := make(map[string]string) // hashed type -> type
	for t := range pool.caps {
		typeNames[hashContent(t)] = t
	}
	linkPreds := make(map[string]bool)
	for p := range individualPredicates(ontology) {
		linkPreds[hashContent(p)] = true
	}

	var scanned int
	types := make(map[string]string) // entity -> type, for shareable types
	individualLinks := make(map[string]int)

	err := scan(func(t *model.Triple) error {
		scanned++
		// Unhashed content from earlier runs carries its counter
		for _, id := range []string{t.Subject, t.Object} {
			if n := idCounterValue(id); n > maxID {
				maxID = n
			}
		}
		switch {
		case t.Predicate == isType:
			if typ, ok := typeNames[t.Object]; ok {
				types[t.Subject] = typ
			} else if t.Object == household && len(sharedHouseholds) < maxPoolPerType {
				sharedHouseholds[t.Subject] = t.Subject
			}
		case linkPreds[t.Predicate]:
			individualLinks[t.Object]++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	idCounter.Store(maxID)
	for id, count := range individualLinks {
		if typ, ok := types[id]; ok {
			pool.add(typ, id, count, rng)
		}
	}
	return scanned, nil
}

// ---------------------------------------------------------------------------
// Graph generation
// ---------------------------------------------------------------------------

type generatedGraph struct {
	triples []*model.Triple
	// Track generated entities by type for cross-linking
	entities map[string][]string
}

func generateIndividualGraph(
	individualID string,
	contentTypes map[string]map[string][]PropertyMeta,
	ontology []OntologyRelation,
	now int64,
	rng *rand.Rand,
	sharedHouseholds map[string]string, // email -> householdID for cross-linking
	mu *sync.Mutex,
	pool *sharePool,
) *generatedGraph {

	g := &generatedGraph{
		entities: make(map[string][]string),
	}

	// Generate the individual entity
	indProps := contentTypes["INDIVIDUAL"]
	g.triples = append(g.triples, generateEntityTriples(individualID, "INDIVIDUAL", indProps, now)...)
	g.entities["INDIVIDUAL"] = []string{individualID}

	// Entities linked to this individual (new or shared), for relationship wiring
	generated := map[string][]string{
		"INDIVIDUAL": {individualID},
	}
	// Entities created by this individual. Only these get their own outgoing
	// relations; shared entities already have theirs.
	created := map[string][]string{}
	linked := map[string]bool{}

	// Process ontology rules originating from INDIVIDUAL
	for _, rel := range ontology {
		if rel.Sub != "INDIVIDUAL" {
			continue
		}

		card := predicateCardinality[rel.Pred]
		count := card.min
		if card.max > card.min {
			count = card.min + rng.Intn(card.max-card.min+1)
		}

		objType := rel.Obj

		// Special handling for HOUSEHOLD: try to share via email
		if objType == "HOUSEHOLD" {
			householdID := tryShareHousehold(individualID, indProps, sharedHouseholds, mu, rng)
			if householdID == "" {
				householdID = generateEntityID("HOUSEHOLD")
				registerHouseholdEmails(householdID, indProps, sharedHouseholds, mu)
				if hhProps, ok := contentTypes["HOUSEHOLD"]; ok {
					g.triples = append(g.triples, generateEntityTriples(householdID, "HOUSEHOLD", hhProps, now)...)
				}
				created["HOUSEHOLD"] = []string{householdID}
			}
			generated["HOUSEHOLD"] = []string{householdID}

			g.triples = append(g.triples, &model.Triple{
				Subject:   individualID,
				Predicate: hashContent(rel.Pred),
				Object:    householdID,
				Props: map[string]interface{}{
					"created":   now,
					"last_seen": now,
				},
			})
			continue
		}

		for i := 0; i < count; i++ {
			// Link to an existing entity when sharing, otherwise create one
			objID, shared := pool.pick(objType, linked, rng)
			if !shared {
				objID = generateEntityID(objType)
				if objProps, ok := contentTypes[objType]; ok {
					g.triples = append(g.triples, generateEntityTriples(objID, objType, objProps, now)...)
				}
				created[objType] = append(created[objType], objID)
				pool.add(objType, objID, 1, rng)
			}

			linked[objID] = true
			generated[objType] = append(generated[objType], objID)

			// Create the relationship triple
			g.triples = append(g.triples, &model.Triple{
				Subject:   individualID,
				Predicate: hashContent(rel.Pred),
				Object:    objID,
				Props: map[string]interface{}{
					"created":   now,
					"last_seen": now,
				},
			})
		}
	}

	// Process non-INDIVIDUAL ontology rules (e.g., ACCOUNT -> HAS -> CREDIT_DEVICE)
	for _, rel := range ontology {
		if rel.Sub == "INDIVIDUAL" || rel.Sub == "HOUSEHOLD" {
			continue
		}

		subjects, ok := created[rel.Sub]
		if !ok {
			continue
		}

		for _, subID := range subjects {
			card := predicateCardinality[rel.Pred]
			count := card.min
			if card.max > card.min {
				count = card.min + rng.Intn(card.max-card.min+1)
			}

			// Try to reuse already-generated objects of this type
			existingObjs := generated[rel.Obj]
			for i := 0; i < count; i++ {
				var objID string
				if i < len(existingObjs) && rng.Float64() < 0.5 {
					objID = existingObjs[i]
				} else {
					objID = generateEntityID(rel.Obj)
					if objProps, ok := contentTypes[rel.Obj]; ok {
						g.triples = append(g.triples, generateEntityTriples(objID, rel.Obj, objProps, now)...)
					}
					generated[rel.Obj] = append(generated[rel.Obj], objID)
				}

				g.triples = append(g.triples, &model.Triple{
					Subject:   subID,
					Predicate: hashContent(rel.Pred),
					Object:    objID,
					Props: map[string]interface{}{
						"created":   now,
						"last_seen": now,
					},
				})
			}
		}
	}

	// Process HOUSEHOLD ontology rules (HOUSEHOLD -> HAS_AN -> ADDRESS, etc.)
	for _, rel := range ontology {
		if rel.Sub != "HOUSEHOLD" {
			continue
		}
		if rel.Obj == "INDIVIDUAL" {
			// Reverse link already exists via IS_PART_OF
			continue
		}

		subjects, ok := created["HOUSEHOLD"]
		if !ok {
			continue
		}

		for _, subID := range subjects {
			card := predicateCardinality[rel.Pred]
			count := card.min
			if card.max > card.min {
				count = card.min + rng.Intn(card.max-card.min+1)
			}

			// Reuse ADDRESS if individual already has one
			existingObjs := generated[rel.Obj]
			for i := 0; i < count; i++ {
				var objID string
				if i < len(existingObjs) && rng.Float64() < 0.7 {
					objID = existingObjs[i]
				} else {
					objID = generateEntityID(rel.Obj)
					if objProps, ok := contentTypes[rel.Obj]; ok {
						g.triples = append(g.triples, generateEntityTriples(objID, rel.Obj, objProps, now)...)
					}
				}

				g.triples = append(g.triples, &model.Triple{
					Subject:   subID,
					Predicate: hashContent(rel.Pred),
					Object:    objID,
					Props: map[string]interface{}{
						"created":   now,
						"last_seen": now,
					},
				})
			}
		}
	}

	return g
}

// tryShareHousehold checks if this individual shares an email/phone with an existing household.
func tryShareHousehold(individualID string, indProps map[string][]PropertyMeta, shared map[string]string, mu *sync.Mutex, rng *rand.Rand) string {
	// 20% chance of sharing a household with another individual (simulates family)
	if rng.Float64() > 0.2 {
		return ""
	}
	mu.Lock()
	defer mu.Unlock()
	for hhID := range shared {
		// Pick a random existing household
		return shared[hhID]
	}
	return ""
}

func registerHouseholdEmails(householdID string, indProps map[string][]PropertyMeta, shared map[string]string, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	// Keep map bounded to avoid unbounded memory growth
	if len(shared) > 100000 {
		return
	}
	shared[householdID] = householdID
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	host := flag.String("host", "127.0.0.1", "Aerospike host")
	port := flag.Int("port", 3000, "Aerospike port")
	namespace := flag.String("namespace", "test", "Aerospike namespace")
	count := flag.Int("count", 1000, "Number of INDIVIDUALs to generate")
	batchSize := flag.Int("batch", 256, "Batch write size")
	workers := flag.Int("workers", 8, "Number of concurrent writer goroutines")
	contentTypesPath := flag.String("content-types", "descriptors/contentTypes.json", "Path to contentTypes.json")
	ontologyPath := flag.String("ontology", "descriptors/ontology.json", "Path to ontology.json")
	cardinalitiesPath := flag.String("cardinalities", "descriptors/cardinalities.json", "Path to cardinalities.json")
	shareRate := flag.Float64("share-rate", 0.2, "Probability an individual links to an existing entity instead of a new one")
	hashType := flag.String("hash-type", "no-hash", "Hash all content: no-hash, sha-256, or sha-512")
	flag.Parse()

	var err error
	if hashContent, err = newHashFunc(*hashType); err != nil {
		log.Fatal(err)
	}

	// Load descriptors
	ctData, err := os.ReadFile(*contentTypesPath)
	if err != nil {
		log.Fatalf("failed to read content types: %v", err)
	}
	var ctFile ContentTypesFile
	if err := json.Unmarshal(ctData, &ctFile); err != nil {
		log.Fatalf("failed to parse content types: %v", err)
	}

	ontData, err := os.ReadFile(*ontologyPath)
	if err != nil {
		log.Fatalf("failed to read ontology: %v", err)
	}
	var ontFile OntologyFile
	if err := json.Unmarshal(ontData, &ontFile); err != nil {
		log.Fatalf("failed to parse ontology: %v", err)
	}

	cardData, err := os.ReadFile(*cardinalitiesPath)
	if err != nil {
		log.Fatalf("failed to read cardinalities: %v", err)
	}
	var cardFile CardinalitiesFile
	if err := json.Unmarshal(cardData, &cardFile); err != nil {
		log.Fatalf("failed to parse cardinalities: %v", err)
	}

	fmt.Printf("Loaded %d content types, %d ontology relations, %d cardinalities (hash: %s)\n",
		len(ctFile.ContentTypes), len(ontFile.Ontology), len(cardFile.Cardinalities), *hashType)
	for ct, props := range ctFile.ContentTypes {
		fmt.Printf("  %-20s %d identifiers\n", ct, len(props))
	}
	fmt.Println()

	// Connect to Aerospike
	gs, err := store.NewGraphStore(*host, *port, *namespace)
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer gs.Close()

	if err := gs.CreateIndexes(); err != nil {
		log.Fatalf("failed to create indexes: %v", err)
	}

	pool := newSharePool(*shareRate, cardFile.Cardinalities)
	sharedHouseholds := make(map[string]string)
	fmt.Println("Scanning existing graph for shareable entities...")
	savedID, err := gs.GetCounter(idCounterName)
	if err != nil {
		log.Fatalf("failed to read ID counter: %v", err)
	}
	scanned, err := loadExisting(gs.ScanEach, savedID, pool, ontFile.Ontology, sharedHouseholds, rand.New(rand.NewSource(rand.Int63())))
	if err != nil {
		log.Fatalf("failed to scan existing graph: %v", err)
	}
	fmt.Printf("  %d existing triples, IDs resume at %d, %d households\n", scanned, idCounter.Load()+1, len(sharedHouseholds))
	for _, c := range cardFile.Cardinalities {
		fmt.Printf("  %-20s max %d %s, %d shareable\n", c.Type, c.Max, c.LinkedTo, len(pool.avail[c.Type]))
	}
	fmt.Println()

	fmt.Printf("Generating %d individuals with full entity graphs...\n", *count)

	// Pipeline: generate → batch → write
	tripleCh := make(chan []*model.Triple, *workers*4)
	var writeWg sync.WaitGroup
	var triplesWritten atomic.Int64
	var batchErrors atomic.Int64

	// Writer goroutines
	for w := 0; w < *workers; w++ {
		writeWg.Add(1)
		go func() {
			defer writeWg.Done()
			buffer := make([]*model.Triple, 0, *batchSize)
			for batch := range tripleCh {
				buffer = append(buffer, batch...)
				for len(buffer) >= *batchSize {
					if err := gs.BatchPutTriples(buffer[:*batchSize]); err != nil {
						batchErrors.Add(1)
						log.Printf("batch write error: %v", err)
					}
					triplesWritten.Add(int64(*batchSize))
					buffer = append(buffer[:0], buffer[*batchSize:]...)
				}
			}
			// Flush remaining
			if len(buffer) > 0 {
				if err := gs.BatchPutTriples(buffer); err != nil {
					batchErrors.Add(1)
					log.Printf("batch write error: %v", err)
				}
				triplesWritten.Add(int64(len(buffer)))
			}
		}()
	}

	// Progress reporter
	// Persist the ID high-water mark so later runs never reuse IDs, even
	// when content is hashed and IDs can't be read back from the data.
	saveIDCounter := func() {
		if err := gs.SetCounter(idCounterName, idCounter.Load()); err != nil {
			log.Printf("failed to save ID counter: %v", err)
		}
	}

	startTime := time.Now()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				written := triplesWritten.Load()
				elapsed := time.Since(startTime).Seconds()
				rate := float64(written) / elapsed
				fmt.Printf("\r  triples written: %d  (%.0f/sec)  errors: %d",
					written, rate, batchErrors.Load())
				saveIDCounter()
			case <-done:
				return
			}
		}
	}()

	// Generator goroutines
	var genWg sync.WaitGroup
	genCh := make(chan int, *workers*2)
	var hhMu sync.Mutex
	now := time.Now().UnixMilli()

	for w := 0; w < *workers; w++ {
		genWg.Add(1)
		go func(seed int64) {
			defer genWg.Done()
			rng := rand.New(rand.NewSource(seed))
			for range genCh {
				indID := generateEntityID("INDIVIDUAL")
				g := generateIndividualGraph(indID, ctFile.ContentTypes, ontFile.Ontology, now, rng, sharedHouseholds, &hhMu, pool)
				tripleCh <- g.triples
			}
		}(rand.Int63())
	}

	// Feed individual IDs
	for i := 0; i < *count; i++ {
		genCh <- i
	}
	close(genCh)
	genWg.Wait()
	saveIDCounter()
	close(tripleCh)
	writeWg.Wait()
	close(done)

	elapsed := time.Since(startTime)
	written := triplesWritten.Load()
	rate := float64(written) / elapsed.Seconds()
	fmt.Printf("\n\nDone. Generated %d individuals → %d triples in %s (%.0f triples/sec)\n",
		*count, written, elapsed.Round(time.Millisecond), rate)
	if errors := batchErrors.Load(); errors > 0 {
		fmt.Printf("  batch errors: %d\n", errors)
	}
}
