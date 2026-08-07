package statebranch

// When a state key is allowed to disappear.
//
// The state branch is append-only: publication fails if an ID restored at the
// start of a run is missing at the end. That rule exists because the failure it
// prevents is catastrophic and silent — a run that loses records re-notifies
// the user about jobs they already read, and the same bug repeats every cycle.
//
// It is also, as written, absolute, and one thing legitimately needs to move
// keys: the transport-in-the-key repair (see internal/source/staterekey.go).
// 39,410 of 71,582 keys embedded a vendor hostname that the vendor changes
// unilaterally, and while those keys stay, editing `host` after a shard move
// orphans a whole board's history.
//
// So this file replaces "no removals" with "no UNEXPLAINED removals". Four
// explanations are accepted, and each one is checkable from the two state files
// alone — no config, no clock, no network, and no generic allow_shrink switch:
//
//  1. REKEY. source.RekeyStateKey(id) is a frozen pure function of the old key.
//     If it names a different key and that key is present in the candidate with
//     `notified` preserved, the record moved; it did not vanish.
//  2. JUNK. source.IsJunkStateKey(id) matches 18 keys that are a Workday fetch
//     base URL with no posting path — records no adapter alive can produce, and
//     that were never a posting. Only while un-notified.
//  3. MARKER. A "__jobwatch_source__/" record is derived bookkeeping, not a
//     posting: it says "we have adopted this board". One may be dropped only
//     when the candidate proves the board it named owns no surviving history,
//     which is exactly the census's sweep condition.
//  4. DECLARED PREFIX MOVE. A marker in the CANDIDATE may declare the prefix it
//     absorbed (previous_state_prefix, the config escape hatch for an ATS move
//     the rules cannot infer). That declaration authorizes moving keys from the
//     old prefix to the marker's own — again only when the destination is
//     present with `notified` preserved.
//
// Every rule ends at the same question: is the record still there, under a key
// this file can name, with its delivery flag intact? A removal that cannot
// answer it still fails the push, loudly, before anything is written.

import (
	"fmt"
	"strings"

	"jobwatch/internal/source"
)

// reservedKeyPrefix marks runner bookkeeping keys. Duplicated from the runner
// rather than imported for the same reason the health window caps are: the
// point of validation is that a producer bug cannot certify itself.
const reservedKeyPrefix = "__jobwatch_"

// checkRemovals reports the first removal the candidate cannot explain.
func checkRemovals(baseRecords, candidateRecords map[string]stateRecord) error {
	moves, err := declaredPrefixMoves(candidateRecords)
	if err != nil {
		return err
	}
	for id, baseRecord := range baseRecords {
		if _, exists := candidateRecords[id]; exists {
			continue
		}
		if err := explainRemoval(id, baseRecord, candidateRecords, moves); err != nil {
			return err
		}
	}
	return nil
}

// declaredPrefixMoves maps each absorbed prefix to the prefix that absorbed it.
// Two markers claiming the same old prefix is not a move but a collision: the
// records would have two destinations and no way to choose, so it is refused
// rather than resolved.
func declaredPrefixMoves(records map[string]stateRecord) (map[string]string, error) {
	var moves map[string]string
	for id, record := range records {
		if record.previousStatePrefix == "" {
			continue
		}
		if moves == nil {
			moves = make(map[string]string, 1)
		}
		if existing, dup := moves[record.previousStatePrefix]; dup && existing != record.statePrefix {
			return nil, fmt.Errorf("candidate state declares prefix %q moving to both %q and %q (see %s)",
				record.previousStatePrefix, existing, record.statePrefix, id)
		}
		moves[record.previousStatePrefix] = record.statePrefix
	}
	return moves, nil
}

func explainRemoval(id string, base stateRecord, candidate map[string]stateRecord, moves map[string]string) error {
	if strings.HasPrefix(id, sourceMarkerKeyPrefix) {
		// A marker carries no delivery fact, so this can only ever be true;
		// asserting it anyway keeps the exemption from widening if that ever
		// stops being so.
		if base.notified {
			return fmt.Errorf("candidate state removed notified source marker %q", id)
		}
		if base.statePrefix == "" {
			// A marker written before prefixes were recorded. It cannot prove
			// what it owned, and it cannot be re-derived either (identities are
			// not parseable), so the only alternative to allowing this is a
			// state branch that can never drop a pre-schema marker again.
			return nil
		}
		if hasPostingUnderPrefix(candidate, base.statePrefix) {
			return fmt.Errorf("candidate state removed source marker %q while %d record(s) still live under %q",
				id, countPostingsUnderPrefix(candidate, base.statePrefix), base.statePrefix)
		}
		return nil
	}
	if strings.HasPrefix(id, reservedKeyPrefix) {
		// Health records, the seed-in-progress markers and the source registry
		// are load-bearing state the runner never deletes. Nothing explains
		// their disappearance, so nothing may.
		return fmt.Errorf("candidate state removed existing job ID %q (runner bookkeeping is never dropped)", id)
	}
	if source.IsJunkStateKey(id) {
		if base.notified {
			return fmt.Errorf("candidate state removed notified job ID %q", id)
		}
		return nil
	}
	for _, target := range removalTargets(id, moves) {
		moved, exists := candidate[target]
		if !exists {
			continue
		}
		if base.notified && !moved.notified {
			return fmt.Errorf("candidate state moved job ID %q to %q and rolled it back to pending", id, target)
		}
		return nil
	}
	return fmt.Errorf("candidate state removed existing job ID %q", id)
}

// removalTargets lists every key a removed record is allowed to have moved to.
//
// The two hops compose deliberately. A board that was BOTH repaired by the
// frozen rules and then moved by a declared prefix swap — the exact sequence a
// user follows after pasting previous_state_prefix out of a migration email —
// leaves keys that match neither hop on its own.
func removalTargets(id string, moves map[string]string) []string {
	hops := []string{id}
	if rekeyed := source.RekeyStateKey(id); rekeyed != id {
		hops = append(hops, rekeyed)
	}
	targets := make([]string, 0, 1+2*len(moves))
	for _, hop := range hops {
		if hop != id {
			targets = append(targets, hop)
		}
		for from, to := range moves {
			if from != "" && strings.HasPrefix(hop, from) {
				targets = append(targets, to+strings.TrimPrefix(hop, from))
			}
		}
	}
	return targets
}

// hasPostingUnderPrefix and countPostingsUnderPrefix mirror the store's own
// prefix predicates. They are re-implemented here on purpose: this is the gate,
// and a gate that shares its arithmetic with the writer it is checking cannot
// catch the writer being wrong.
func hasPostingUnderPrefix(records map[string]stateRecord, prefix string) bool {
	if prefix == "" {
		return false
	}
	for id := range records {
		if !strings.HasPrefix(id, reservedKeyPrefix) && strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

func countPostingsUnderPrefix(records map[string]stateRecord, prefix string) int {
	if prefix == "" {
		return 0
	}
	n := 0
	for id := range records {
		if !strings.HasPrefix(id, reservedKeyPrefix) && strings.HasPrefix(id, prefix) {
			n++
		}
	}
	return n
}
