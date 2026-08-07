package source

// One-time, in-place repair of state keys that baked a vendor's transport
// coordinate into a board's history.
//
// WHY THIS EXISTS
//
// Until this change, seven adapters put a hostname (or an equivalent
// vendor-side routing token) into every job ID they produced. When an ATS
// moved a tenant between shards — which the vendor does unilaterally — the
// user's only recourse was to edit `host` in the config, and that single edit
// changed the board's identity, its state prefix, and every one of its job
// IDs at once. All three "have I seen this board?" guards missed together, so
// the board was silently re-baselined: no email from it, and 100% of its
// history orphaned. 57 workday boards across 8 shards are ~47% of all state.
//
// coordsFor now excludes those params from identity and from job IDs. That
// fixes new keys. These rules fix the 71,582 keys already on disk.
//
// THE RULES ARE FROZEN
//
// Each rule was simulated against the real state file: 39,410 keys move, 0
// collisions. The counts below are those measurements. Do not "improve" a
// pattern later — a rule is a promise about keys that were written by code
// that no longer exists, and the only evidence those keys were ever produced
// is the state file itself.
//
// The rules are PURE FUNCTIONS OF THE OLD KEY. They never read config. That
// matters for the case the user has already hit: someone whose host edit
// orphaned a board months ago still has both halves of that board on disk,
// and the rules repair both, because neither half depends on what `host` says
// today.
//
// IDEMPOTENT BY CONSTRUCTION
//
// There is no schema version stamped anywhere, and none is needed: every
// rule's OUTPUT lies outside its own INPUT language, so a second pass matches
// nothing. Each rule states its own argument below, and
// TestRekeyRulesAreIdempotent feeds every output (plus a current-format key
// per registered adapter) back through the whole table and demands zero
// rewrites. That test is the thing the no-schema-version claim rests on.

import (
	"regexp"
	"strings"
)

// reservedKeyPrefix marks runner bookkeeping keys (source markers, seed
// progress, health records). They are NOT migrated.
//
// A source marker is a derived cache of "we have adopted this board", and its
// key embeds the OLD identity. Rewriting one would mean parsing an identity
// string — and identities are not parseable: a real stored marker is
// literally "__jobwatch_source__/eightfold/jobs.ericsson.com/ericsson.com//query=Cradlepoint",
// double slash and all. Once the posting keys move, HasPostingPrefix alone
// answers "is this board known?" correctly and the runner writes a fresh
// marker under the new identity. The stale ones are swept by the census pass.
const reservedKeyPrefix = "__jobwatch_"

type rekeyRule struct {
	// srcType is the adapter whose keys this rule repairs, and prefix is the
	// cheap gate that keeps 71,582 keys from paying for seven regexes each.
	srcType string
	prefix  string
	pattern *regexp.Regexp
	replace string
}

// rekeyRules is the frozen table. Measured moves against the live state file
// are noted per rule.
var rekeyRules = []rekeyRule{
	{
		// 34,241 keys. Old IDs were literally "workday/" + the fetch base URL
		// + externalPath, so the shard host sat in the middle of every key:
		//   workday/https://redhat.wd5.myworkdayjobs.com/wday/cxs/redhat/jobs/job/Bengaluru/Engineer_R-1
		// -> workday/redhat/jobs/job/Bengaluru/Engineer_R-1
		//
		// Idempotent: the output's first component is a Workday TENANT slug.
		// For the output to re-match, that component would have to be the
		// literal "https:" AND be followed by an empty site component — but
		// the pattern captures site as [^/]+, which cannot be empty.
		srcType: "workday",
		prefix:  "workday/",
		pattern: regexp.MustCompile(`^workday/https://[^/]+/wday/cxs/([^/]+)/([^/]+)/(.+)$`),
		replace: `workday/${1}/${2}/${3}`,
	},
	{
		// 4,815 keys. Only the DNS SUFFIX was transport: in
		// "eofe.fa.us2.oraclecloud.com" the first label is the customer pod
		// and the rest is the Oracle data-centre region that pods migrate
		// between.
		//   oraclece/eofe.fa.us2.oraclecloud.com/BNY-Careers/12345
		// -> oraclece/eofe/BNY-Careers/12345
		//
		// Idempotent: the input demands a dot in the first component
		// (([^./]+)\.[^/]+) and the output's first component is the capture,
		// which by construction contains no dot.
		srcType: "oraclece",
		prefix:  "oraclece/",
		pattern: regexp.MustCompile(`^oraclece/([^./]+)\.[^/]+/([^/]+)/(.+)$`),
		replace: `oraclece/${1}/${2}/${3}`,
	},
	{
		// 12 keys. recruiting.ultipro.com / recruiting2 / recruiting5 are
		// UKG's own numbered clusters.
		//   ukg/recruiting2.ultipro.com/PRO1053PROC/d6eed263-.../a1b2...
		// -> ukg/PRO1053PROC/d6eed263-.../a1b2...
		//
		// Idempotent: the input demands a dotted first component, and the
		// output's first component is a UKG tenant code, which the adapter
		// has always validated as ^[A-Za-z0-9_-]+$ — no dots possible.
		srcType: "ukg",
		prefix:  "ukg/",
		pattern: regexp.MustCompile(`^ukg/[^/]+\.[^/]+/([^/]+)/([^/]+)/(.+)$`),
		replace: `ukg/${1}/${2}/${3}`,
	},
	{
		// 159 keys. Eightfold serves one board from either a vanity host
		// (jobs.ericsson.com) or its own (morganstanley.eightfold.ai); the
		// `domain` is the employer key its API is queried with.
		//   eightfold/jobs.ericsson.com/ericsson.com/12345
		// -> eightfold/ericsson.com/12345
		//
		// Idempotent by ARITY, not by shape: the input needs three
		// slash-separated components after "eightfold/" and the output has
		// two. Position IDs are decimal integers, so the tail can never
		// contribute a third.
		srcType: "eightfold",
		prefix:  "eightfold/",
		pattern: regexp.MustCompile(`^eightfold/[^/]+/([^/]+)/(.+)$`),
		replace: `eightfold/${1}/${2}`,
	},
	{
		// 132 keys. The career-site domain is the employer's marketing DNS
		// and moves on a rebrand; company_id is Zwayam's account number.
		//   zwayam/careers.cult.fit/15470/98765
		// -> zwayam/15470/98765
		//
		// Idempotent: the input demands a dotted first component; the output's
		// is [0-9]+.
		srcType: "zwayam",
		prefix:  "zwayam/",
		pattern: regexp.MustCompile(`^zwayam/[^/]+\.[^/]+/([0-9]+)/(.+)$`),
		replace: `zwayam/${1}/${2}`,
	},
	{
		// 23 keys. appid/scope name IBM's Watson Discovery search collection
		// ("careers"/"careers2"), which IBM has already renumbered once.
		//   ibm/careers/careers2/in/12345BR
		// -> ibm/in/12345BR
		//
		// Idempotent by arity: the input needs four components, the output
		// has two. IBM external IDs match ^[A-Za-z0-9][A-Za-z0-9._-]*$, so
		// they never add components.
		srcType: "ibm",
		prefix:  "ibm/",
		pattern: regexp.MustCompile(`^ibm/[^/]+/[^/]+/([^/]+)/(.+)$`),
		replace: `ibm/${1}/${2}`,
	},
	{
		// 28 keys. request_type is an opaque token HROne regenerates when a
		// customer reconfigures their career portal.
		//   hrone/addverb/UVozgs-AUV1ILPLBxDlf7A/P3WIS.../abc123
		// -> hrone/addverb/P3WIS.../abc123
		//
		// Idempotent by arity: the input needs four components, the output
		// has three. Encrypted position IDs are ^[A-Za-z0-9_-]+$.
		srcType: "hrone",
		prefix:  "hrone/",
		pattern: regexp.MustCompile(`^hrone/([^/]+)/[^/]+/([^/]+)/(.+)$`),
		replace: `hrone/${1}/${2}/${3}`,
	},
}

// junkWorkdayBaseKey matches a workday key that is exactly the fetch base URL
// with NO externalPath after it.
//
// 18 such keys exist. They are provably unrecreatable: the adapter appends a
// non-empty externalPath to every ID it emits and skips postings whose
// externalPath is missing, so nothing in today's code can produce one. Their
// titles ("Citi: ", "Barclays: ") show what they were — a long-dead
// normalization that recorded the board itself as a posting. They are dropped
// rather than moved because moving them would mint a bare board-prefix key
// that CountPostingPrefix would then read as "this board has history",
// silently defeating the census.
var junkWorkdayBaseKey = regexp.MustCompile(`^workday/https://[^/]+/wday/cxs/[^/]+/[^/]+$`)

// RekeyStateKey returns the key a stored posting should live under today. It
// returns id UNCHANGED when no rule applies, which is the answer for the
// ~44% of keys that never embedded transport and for every key already in
// current format.
//
// It is a pure function: same input, same output, no config, no clock.
func RekeyStateKey(id string) string {
	if strings.HasPrefix(id, reservedKeyPrefix) {
		return id
	}
	for _, rule := range rekeyRules {
		if !strings.HasPrefix(id, rule.prefix) {
			continue
		}
		if !rule.pattern.MatchString(id) {
			// A key under this adapter's namespace that does not match is
			// already migrated (or was never in the old shape). Later rules
			// gate on other prefixes, so stopping here costs nothing.
			return id
		}
		return rule.pattern.ReplaceAllString(id, rule.replace)
	}
	return id
}

// IsJunkStateKey reports whether id is a stored key that no adapter can
// produce any more and that carries no posting. See junkWorkdayBaseKey.
//
// Deleting state is otherwise forbidden (a posting's presence IS the
// "already emailed" fact), so this predicate is deliberately narrow: it
// matches one shape, from one adapter, that provably has no externalPath.
func IsJunkStateKey(id string) bool {
	if strings.HasPrefix(id, reservedKeyPrefix) {
		return false
	}
	return junkWorkdayBaseKey.MatchString(id)
}
