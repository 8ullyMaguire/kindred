package web

// The /mirror page: what this instance actually holds.
//
// The nav pointed readers at /stats and /healthz. Both are real endpoints, both
// are the API's, and both were presented as pages -- so clicking "Stats" got a
// wall of minified JSON. The API is this project's product, so /stats keeps its
// contract and its tests; this is the page a reader was asking for.
//
// The facts worth the page are the ones ONLY JSON could show before: how many
// tags the mirror knows, how many works carry them, and how large the
// co-occurrence graph is. A recommendation is a walk over that graph, so its
// size is the difference between a useful answer and a thin one -- and a thin
// graph is not an error. It returns confident nonsense, which is exactly the
// kind of failure worth putting on a page instead of behind an endpoint nobody
// curls.

import (
	"net/http"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// MirrorPage is the /mirror view.
type MirrorPage struct {
	Base
	// NoCorpus means there is no corpus handle at all, which is a different
	// fact from a corpus that could not be counted. Both print "not known";
	// neither prints 0.
	NoCorpus  bool
	HasCorpus bool
	Works     int
	Tags      int
	WorkTags  int
	Edges     int

	// Graph is the co-occurrence CSR. Absent when the deployment never loaded
	// one, which is not the same as an empty one: an empty graph was built and
	// found nothing, an absent graph was never built.
	HasGraph   bool
	Nodes      int
	GraphEdges int
	GraphBytes int64

	// Backlog, from the crawl queue.
	NoQueue  bool
	QueueErr bool
	Pending  int
	Done     int
	Parked   int
	Cap      int
	Sources  []QueueSource
}

func (d Deps) renderMirror(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := MirrorPage{Base: d.baseOf(r, "This mirror", "What this mirror holds")}

	if d.Engine == nil || d.Engine.Corpus == nil {
		page.NoCorpus = true
	} else {
		page.HasCorpus = true
		// A failed count is not a count of zero. The page says it cannot say
		// rather than printing 0, because "0 tags" is a claim about the data
		// and this is an absence of information.
		page.Works, _ = d.Engine.Corpus.CountWorks(ctx)
		page.Tags, _ = d.Engine.Corpus.CountTags(ctx)
		page.WorkTags, _ = d.Engine.Corpus.CountWorkTags(ctx)
		page.Edges, _ = d.Engine.Corpus.CountEdges(ctx)
		if d.Engine.Graph != nil {
			gs := d.Engine.Graph.Stats()
			page.HasGraph = true
			page.Nodes, page.GraphEdges, page.GraphBytes = gs.NodeCount, gs.EdgeCount, gs.Bytes
		}
	}

	page.Cap = store.CrawlMaxAttempts
	switch {
	case d.Queue == nil:
		page.NoQueue = true
	default:
		st, err := d.Queue.CrawlQueueStats(ctx, 0)
		if err != nil {
			// THREE states, spelled three ways. Collapsing a failed query into
			// "no queue" would print the reassuring sentence for a deployment
			// whose backlog is unknown, and an unknown backlog is the state
			// where the mirror might have stopped growing without saying so.
			page.QueueErr = true
			break
		}
		page.Pending, page.Done, page.Parked = st.Pending, st.Done, st.Parked
		page.Sources = sortedQueueSources(st.BySource)
	}

	d.page(w, "mirror.html", page, http.StatusOK)
}
