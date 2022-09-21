package handlers

import (
	"eth2-exporter/services"
	"eth2-exporter/templates"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"html/template"
	"net/http"

	"github.com/gorilla/mux"
)

var chartsTemplate = template.Must(template.New("charts").Funcs(utils.GetTemplateFuncs()).ParseFS(templates.Files, "layout.html", "charts.html"))
var genericChartTemplate = template.Must(template.New("chart").Funcs(utils.GetTemplateFuncs()).ParseFS(templates.Files, "layout.html", "genericchart.html"))
var chartsUnavailableTemplate = template.Must(template.New("chart").Funcs(utils.GetTemplateFuncs()).ParseFS(templates.Files, "layout.html", "chartsunavailable.html"))
var slotVizTemplate = template.Must(template.New("slotViz").Funcs(utils.GetTemplateFuncs()).ParseFS(templates.Files, "layout.html", "slotViz.html", "slotVizPage.html"))

// Charts uses a go template for presenting the page to show charts
func Charts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")

	data := InitPageData(w, r, "stats", "/charts", "Charts")

	chartsPageData := services.LatestChartsPageData()
	if chartsPageData == nil {
		err := chartsUnavailableTemplate.ExecuteTemplate(w, "layout", data)
		if err != nil {
			logger.Errorf("error executing template for %v route: %v", r.URL.String(), err)
			http.Error(w, "Internal server error", http.StatusServiceUnavailable)
			return
		}
		return
	}

	data.Data = chartsPageData

	chartsTemplate = template.Must(template.New("charts").Funcs(utils.GetTemplateFuncs()).ParseFS(templates.Files, "layout.html", "charts.html"))
	err := chartsTemplate.ExecuteTemplate(w, "layout", data)
	if err != nil {
		logger.Errorf("error executing template for %v route: %v", r.URL.String(), err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// Chart renders a single chart
func Chart(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chartVar := vars["chart"]
	switch chartVar {
	case "slotviz":
		SlotViz(w, r)
	default:
		GenericChart(w, r)
	}
}

// GenericChart uses a go template for presenting the page of a generic chart
func GenericChart(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	chartVar := vars["chart"]

	w.Header().Set("Content-Type", "text/html")
	data := InitPageData(w, r, "stats", "/charts", "Chart")

	chartsPageData := services.LatestChartsPageData()
	if chartsPageData == nil {
		err := chartsUnavailableTemplate.ExecuteTemplate(w, "layout", data)
		if err != nil {
			logger.Errorf("error executing template for %v route: %v", r.URL.String(), err)
			http.Error(w, "Internal server error", http.StatusServiceUnavailable)
			return
		}
		return
	}

	var chartData *types.GenericChartData
	for _, d := range *chartsPageData {
		if d.Path == chartVar {
			chartData = d.Data
			break
		}
	}

	if chartData == nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	SetPageDataTitle(data, fmt.Sprintf("%v Chart", chartData.Title))
	data.Meta.Path = "/charts/" + chartVar
	data.Data = chartData

	genericChartTemplate = template.Must(template.New("chart").Funcs(utils.GetTemplateFuncs()).ParseFS(templates.Files, "layout.html", "genericchart.html"))
	err := genericChartTemplate.ExecuteTemplate(w, "layout", data)
	if err != nil {
		logger.Errorf("error executing template for %v route: %v", r.URL.String(), err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// SlotViz renders a single page with a d3 slot (block) visualisation
func SlotViz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	data := InitPageData(w, r, "stats", "/charts", "Charts")

	if utils.Config.Frontend.Debug {
		slotVizTemplate = template.Must(template.New("slotViz").Funcs(utils.GetTemplateFuncs()).ParseFS(templates.Files, "layout.html", "slotViz.html", "slotVizPage.html"))
	}
	slotVizData := types.SlotVizPageData{
		Selector: "checklist",
		Epochs:   services.LatestSlotVizMetrics(),
	}
	data.Data = slotVizData
	err := slotVizTemplate.ExecuteTemplate(w, "layout", data)
	if err != nil {
		logger.Errorf("error executing template for %v route: %v", r.URL.String(), err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}
