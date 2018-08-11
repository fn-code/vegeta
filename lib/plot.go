package vegeta

import (
	"encoding/json"
	"html/template"
	"io"
	"math"
	"strconv"
)

// An HTMLPlot represents an interactive HTML time series
// plot of Result latencies over time.
type HTMLPlot struct {
	title     string
	threshold int
	series    map[string]*attackSeries
}

// attackSeries groups the two timeSeries an attack results in:
// OK and Error data points
type attackSeries struct{ ok, err *timeSeries }

// add adds the given result to the OK timeSeries if the Result
// has no error, or to the Error timeSeries otherwise.
func (as *attackSeries) add(r *Result) {
	var (
		s     **timeSeries
		label string
	)

	if r.Error == "" {
		s, label = &as.ok, "OK"
	} else {
		s, label = &as.err, "Error"
	}

	if *s == nil {
		*s = newTimeSeries(r.Attack, label, r.Timestamp)
	}

	t := uint64(r.Timestamp.Sub((*s).began)) / 1e6 // ns -> ms
	v := r.Latency.Seconds() * 1000

	(*s).add(t, v)
}

// NewHTMLPlot returns an HTMLPlot with the given title,
// downsampling threshold.
func NewHTMLPlot(title string, threshold int) *HTMLPlot {
	return &HTMLPlot{
		title:     title,
		threshold: threshold,
		series:    map[string]*attackSeries{},
	}
}

// Add adds the given Result to the HTMLPlot time series.
func (p *HTMLPlot) Add(r *Result) {
	s, ok := p.series[r.Attack]
	if !ok {
		s = &attackSeries{}
		p.series[r.Attack] = s
	}
	s.add(r)
}

// Close closes the HTML plot for writing.
func (p *HTMLPlot) Close() {
	for _, as := range p.series {
		for _, ts := range []*timeSeries{as.ok, as.err} {
			if ts != nil {
				ts.data.Finish()
			}
		}
	}
}

// WriteTo writes the HTML plot to the give io.Writer.
func (p HTMLPlot) WriteTo(w io.Writer) (n int64, err error) {
	type dygraphsOpts struct {
		Title       string   `json:"title"`
		Labels      []string `json:"labels,omitempty"`
		YLabel      string   `json:"ylabel"`
		XLabel      string   `json:"xlabel"`
		Colors      []string `json:"colors,omitempty"`
		Legend      string   `json:"legend"`
		ShowRoller  bool     `json:"showRoller"`
		LogScale    bool     `json:"logScale"`
		StrokeWidth float64  `json:"strokeWidth"`
	}

	type plotData struct {
		Title         string
		HTML2CanvasJS template.JS
		DygraphsJS    template.JS
		Data          template.JS
		Opts          template.JS
	}

	dp, labels, err := p.data()
	if err != nil {
		return 0, err
	}

	var sz int
	if len(dp) > 0 {
		sz = len(dp) * len(dp[0]) * 12 // heuristic
	}

	data := dp.Append(make([]byte, 0, sz))

	// TODO: Improve colors to be more intutive
	// Green pallette for OK series
	// Red pallette for Error series

	opts := dygraphsOpts{
		Title:       p.title,
		Labels:      labels,
		YLabel:      "Latency (ms)",
		XLabel:      "Seconds elapsed",
		Legend:      "always",
		ShowRoller:  true,
		LogScale:    true,
		StrokeWidth: 1.3,
	}

	optsJSON, err := json.MarshalIndent(&opts, "    ", " ")
	if err != nil {
		return 0, err
	}

	cw := countingWriter{w: w}
	err = plotTemplate.Execute(&cw, &plotData{
		Title:         p.title,
		HTML2CanvasJS: template.JS(asset(html2canvas)),
		DygraphsJS:    template.JS(asset(dygraphs)),
		Data:          template.JS(data),
		Opts:          template.JS(optsJSON),
	})

	return cw.n, err
}

// See http://dygraphs.com/data.html
func (p *HTMLPlot) data() (dataPoints, []string, error) {
	var (
		series []*timeSeries
		count  int
	)

	for _, as := range p.series {
		for _, s := range [...]*timeSeries{as.ok, as.err} {
			if s != nil {
				series = append(series, s)
				count += s.len
			}
		}
	}

	var (
		size   = 1 + len(series)
		nan    = math.NaN()
		labels = make([]string, size)
		data   = make(dataPoints, 0, count)
	)

	labels[0] = "Seconds"

	for i, s := range series {
		points, err := lttb.Downsample(s.len, p.threshold, s.iter())
		if err != nil {
			return nil, nil, err
		}

		for _, p := range points {
			point := make([]float64, size)
			for j := range point {
				point[j] = nan
			}
			point[0], point[i+1] = p.x, p.y
			data = append(data, point)
		}

		labels[i+1] = s.attack + ": " + s.label
	}

	return data, labels, nil
}

type countingWriter struct {
	n int64
	w io.Writer
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

type dataPoints [][]float64

func (ps dataPoints) Append(buf []byte) []byte {
	buf = append(buf, "[\n  "...)

	for i, p := range ps {
		buf = append(buf, "  ["...)

		for j, f := range p {
			if math.IsNaN(f) {
				buf = append(buf, "NaN"...)
			} else {
				buf = strconv.AppendFloat(buf, f, 'f', -1, 64)
			}

			if j < len(p)-1 {
				buf = append(buf, ',')
			}
		}

		if buf = append(buf, "]"...); i < len(ps)-1 {
			buf = append(buf, ",\n  "...)
		}
	}

	return append(buf, "  ]"...)
}

var plotTemplate = template.Must(template.New("plot").Parse(`
<!doctype html>
<html>
<head>
  <title>{{.Title}}</title>
  <meta charset="utf-8">
</head>
<body>
  <div id="latencies" style="font-family: Courier; width: 100%%; height: 600px"></div>
  <button id="download">Download as PNG</button>
	<script>{{.HTML2CanvasJS}}</script>
	<script>{{.DygraphsJS}}</script>
  <script>
  document.getElementById("download").addEventListener("click", function(e) {
    html2canvas(document.body, {background: "#fff"}).then(function(canvas) {
      var url = canvas.toDataURL('image/png').replace(/^data:image\/[^;]/, 'data:application/octet-stream');
      var a = document.createElement("a");
      a.setAttribute("download", "vegeta-plot.png");
      a.setAttribute("href", url);
      a.click();
    });
  });

  var container = document.getElementById("latencies");
  var opts = {{.Opts}};
  var data = {{.Data}};
  var plot = new Dygraph(container, data, opts);
  </script>
</body>
</html>`))