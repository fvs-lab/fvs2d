package main

import "github.com/fvs-lab/fvs2d/internal/engine"

type Tree = engine.Tree
type Node = engine.Node
type LayerSelector = engine.LayerSelector
type Layer = engine.Layer

const WhiteoutPrefix = engine.WhiteoutPrefix

var (
	BuildTree                = engine.BuildTree
	BuildTreeFromRepo        = engine.BuildTreeFromRepo
	BuildMergedTreeFromRepos = engine.BuildMergedTreeFromRepos
	ResolveLayer             = engine.ResolveLayer
	ParseLayerSelector       = engine.ParseLayerSelector
)
