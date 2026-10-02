package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
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

func generateEntityID(contentType string) string {
	return fmt.Sprintf("%s:%d", contentType, nextID())
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
			Predicate: "HAS_IDENTIFIER",
			Object:    fmt.Sprintf("%s:%s", propName, value),
			Props: map[string]interface{}{
				"identifier_type": propName,
				"value":           value,
				"created":         now,
				"last_seen":       now,
				"decay":           meta.Decay,
			},
		})
	}

	// Entity type triple
	triples = append(triples, &model.Triple{
		Subject:   entityID,
		Predicate: "IS_TYPE",
		Object:    contentType,
		Props: map[string]interface{}{
			"created": now,
		},
	})

	return triples
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
) *generatedGraph {

	g := &generatedGraph{
		entities: make(map[string][]string),
	}

	// Generate the individual entity
	indProps := contentTypes["INDIVIDUAL"]
	g.triples = append(g.triples, generateEntityTriples(individualID, "INDIVIDUAL", indProps, now)...)
	g.entities["INDIVIDUAL"] = []string{individualID}

	// Track generated entities for relationship wiring
	generated := map[string][]string{
		"INDIVIDUAL": {individualID},
	}

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
			}

			if _, exists := generated["HOUSEHOLD"]; !exists {
				if hhProps, ok := contentTypes["HOUSEHOLD"]; ok {
					g.triples = append(g.triples, generateEntityTriples(householdID, "HOUSEHOLD", hhProps, now)...)
				}
				generated["HOUSEHOLD"] = []string{householdID}
			}

			g.triples = append(g.triples, &model.Triple{
				Subject:   individualID,
				Predicate: rel.Pred,
				Object:    householdID,
				Props: map[string]interface{}{
					"created":   now,
					"last_seen": now,
				},
			})
			continue
		}

		for i := 0; i < count; i++ {
			objID := generateEntityID(objType)

			// Generate the related entity
			if objProps, ok := contentTypes[objType]; ok {
				g.triples = append(g.triples, generateEntityTriples(objID, objType, objProps, now)...)
			}

			generated[objType] = append(generated[objType], objID)

			// Create the relationship triple
			g.triples = append(g.triples, &model.Triple{
				Subject:   individualID,
				Predicate: rel.Pred,
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

		subjects, ok := generated[rel.Sub]
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
					Predicate: rel.Pred,
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

		subjects, ok := generated["HOUSEHOLD"]
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
					Predicate: rel.Pred,
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
	flag.Parse()

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

	fmt.Printf("Loaded %d content types, %d ontology relations\n", len(ctFile.ContentTypes), len(ontFile.Ontology))
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
			case <-done:
				return
			}
		}
	}()

	// Generator goroutines
	var genWg sync.WaitGroup
	genCh := make(chan int, *workers*2)
	sharedHouseholds := make(map[string]string)
	var hhMu sync.Mutex
	now := time.Now().UnixMilli()

	for w := 0; w < *workers; w++ {
		genWg.Add(1)
		go func(seed int64) {
			defer genWg.Done()
			rng := rand.New(rand.NewSource(seed))
			for range genCh {
				indID := generateEntityID("INDIVIDUAL")
				g := generateIndividualGraph(indID, ctFile.ContentTypes, ontFile.Ontology, now, rng, sharedHouseholds, &hhMu)
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
