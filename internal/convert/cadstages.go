package convert

import (
	"slices"
)

type cadRouteKey struct{ op, from, to string }

var cadStageRoutes = map[cadRouteKey][]string{
	{OperationConvert, "step", "stl"}: {cadStageGuard, cadStageMesh, cadStageJudge},
	{OperationConvert, "iges", "stl"}: {cadStageGuard, cadStageMesh, cadStageJudge},
	{OperationRender, "step", "png"}:  {cadStageGuard, cadStageMesh, cadStageRender, cadStageJudge},
	{OperationRender, "iges", "png"}:  {cadStageGuard, cadStageMesh, cadStageRender, cadStageJudge},
	{OperationRender, "stl", "png"}:   {cadStageMesh, cadStageRender, cadStageJudge},
	{OperationConvert, "dxf", "svg"}:  {cadStageGuard, cadStageDXF, cadStageInk},
	{OperationConvert, "dxf", "pdf"}:  {cadStageGuard, cadStageDXF, cadStageInk, cadStageRsvg, cadStageJudge},
	{OperationRender, "dxf", "png"}:   {cadStageGuard, cadStageDXF, cadStageInk, cadStageRsvg, cadStageJudge},
	{OperationConvert, "dwg", "svg"}:  {cadStageDWG, cadStageGuard, cadStageDXF, cadStageInk},
	{OperationConvert, "dwg", "pdf"}:  {cadStageDWG, cadStageGuard, cadStageDXF, cadStageInk, cadStageRsvg, cadStageJudge},
	{OperationConvert, "dwg", "png"}:  {cadStageDWG, cadStageGuard, cadStageDXF, cadStageInk, cadStageRsvg, cadStageJudge},
	{OperationRender, "dwg", "png"}:   {cadStageDWG, cadStageGuard, cadStageDXF, cadStageInk, cadStageRsvg, cadStageJudge},
}

func CADStages(op, from, to string) ([]string, bool) {
	route, ok := cadStageRoutes[cadRouteKey{op, NormalizeFormat(from), NormalizeFormat(to)}]
	if !ok {
		return nil, false
	}
	return slices.Clone(route), true
}

func CADStageVocabulary() []string {
	var out []string
	for _, route := range cadStageRoutes {
		for _, stage := range route {
			if !slices.Contains(out, stage) {
				out = append(out, stage)
			}
		}
	}
	slices.Sort(out)
	return out
}

type cadStager struct {
	sink  StageSink
	route []string
}

func newCADStager(sink StageSink, op, from, to string) cadStager {
	route, _ := CADStages(op, from, to)
	return cadStager{sink: sink, route: route}
}

func (s cadStager) enter(stage string) {
	if s.sink == nil {
		return
	}
	i := slices.Index(s.route, stage)
	if i < 0 {
		return
	}
	s.sink.Stage(StageUpdate{Stage: stage, Index: i + 1, Count: len(s.route)})
}
