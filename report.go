package main

import "bytes"

// renderReport fills web/report.html with plotly.js and the runs' JSON (json.Marshal already
// escapes <, > and &, so the data cannot close the <script> tag).
func renderReport(runs [][]byte) []byte {
	page := mustRead("web/report.html")
	page = bytes.Replace(page, []byte("/*PLOTLY*/"), mustRead("web/plotly.min.js"), 1)
	data := append(append([]byte("["), bytes.Join(runs, []byte(","))...), ']')
	return bytes.Replace(page, []byte("/*RUNS*/"), data, 1)
}
