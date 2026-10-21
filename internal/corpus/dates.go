package corpus

// ChronologicalUpdateDateDesc is a SQL expression that orders works by
// their update_date in CHRONOLOGICAL order, most recent first.
//
// The mirror stores update_date as TEXT in AO3's "DD Mon YYYY" form, so a
// plain `update_date DESC` sorts it as a string: measured on the live
// mirror, that yields "31 Oct 2025" as the most recent work, while the
// true most recent is "26 Jun 2026". The day is two digits, so the string
// order is dominated by the day-of-month and the month name is never
// compared.
//
// The expression below splits the string into year, month name and day and
// compares them numerically. It is used for every "recent" ordering, so
// that the site's recent-first list and the API's date sort agree with
// the calendar.
//
// The DESCENDING direction is part of the constant, not left to the
// caller. That is deliberate: every use of it is "most recent first", and
// a bare key expression would let a caller append nothing and get
// ASCENDING order -- which is exactly the bug the first draft of this
// constant had, and it was caught only by running the existing sort tests
// (they listed [1,2,3,4,5,6,7,8] instead of [8,7,6,5,4,3,2,1]).
//
// A NULL or empty date becomes '0000-00-00', which sorts LAST in a
// descending list, so an undated work is at the end rather than the
// beginning.
const ChronologicalUpdateDateDesc = `
CASE
	WHEN w.update_date IS NULL OR w.update_date = '' THEN '0000-00-00'
	WHEN instr(w.update_date, ' ') > 0 THEN
		substr(w.update_date, -4) || '-' ||
		CASE substr(w.update_date, instr(w.update_date, ' ') + 1, 3)
			WHEN 'Jan' THEN '01' WHEN 'Feb' THEN '02' WHEN 'Mar' THEN '03'
			WHEN 'Apr' THEN '04' WHEN 'May' THEN '05' WHEN 'Jun' THEN '06'
			WHEN 'Jul' THEN '07' WHEN 'Aug' THEN '08' WHEN 'Sep' THEN '09'
			WHEN 'Oct' THEN '10' WHEN 'Nov' THEN '11' WHEN 'Dec' THEN '12'
			ELSE '00' END || '-' ||
		substr('0' || substr(w.update_date, 1, instr(w.update_date, ' ') - 1), -2)
	ELSE substr(w.update_date, 1, 10)
END DESC`
