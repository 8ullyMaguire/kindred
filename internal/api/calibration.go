package api

// GET /api/v1/calibration — where hits land, from the newest recorded run.
//
// The recommendation page prints a three-band summary of this. The summary has to
// be checkable against something, so this endpoint carries the unrounded numbers
// and every position: a band the page shows as "18%" is 14 of 80 here, and a reader
// who wants to know whether the tail is worth reading can see 4 of 40 at position 2
// rather than take the sentence's word for it.
//
// Two facts this endpoint refuses to conflate, for the same reason the page does:
//
//	calibrated=false + reason     nothing has been recorded yet
//	rate absent in a band        the band had no observations at all
//	rate = 0                     the band was reached and never right
//
// A JSON consumer that reads a missing rate as zero would draw the same conclusion
// from a run that never looked and a recommender that always missed.

import (
	"net/http"

	"git.polarisocial.xyz/kindred/kindred/internal/eval"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

type calibrationBand struct {
	Band     string   `json:"band"`
	Hits     int      `json:"hits"`
	Observed int      `json:"observed"`
	Rate     *float64 `json:"rate,omitempty"`
}

func (s *Server) handleCalibration(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Calibrated bool                 `json:"calibrated"`
		RunID      int64                `json:"run_id,omitempty"`
		K          int                  `json:"k,omitempty"`
		Readers    int                  `json:"readers,omitempty"`
		MinReaders int                  `json:"min_readers"`
		Bands      []calibrationBand    `json:"bands,omitempty"`
		Positions  []store.EvalPosition `json:"positions,omitempty"`
		Reason     string               `json:"reason,omitempty"`
	}{MinReaders: eval.MinCalibrationReaders}

	if s.Store == nil {
		out.Reason = "no state database"
		writeJSONBody(w, out)
		return
	}
	stored, err := s.Store.LatestCalibration(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !stored.OK {
		out.Reason = "no evaluation has recorded per-position hits yet; `kindred eval` writes them"
		writeJSONBody(w, out)
		return
	}
	out.Calibrated = true
	out.RunID = stored.RunID
	out.K = stored.K
	out.Readers = stored.Readers
	out.Positions = stored.Positions
	for _, b := range []struct {
		name     string
		from, to int
	}{{"1-3", 1, 3}, {"4-7", 4, 7}, {"8+", 8, stored.K}} {
		row := calibrationBand{Band: b.name}
		for _, p := range stored.Positions {
			if p.Position >= b.from && p.Position <= b.to {
				row.Hits += p.Hits
				row.Observed += p.Observed
			}
		}
		if row.Observed > 0 {
			rate := float64(row.Hits) / float64(row.Observed)
			row.Rate = &rate
		}
		out.Bands = append(out.Bands, row)
	}
	writeJSONBody(w, out)
}
