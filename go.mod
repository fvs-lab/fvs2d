module fvs2d

go 1.21

replace fvs-v2-core => ../core

require fvs-v2-core v0.0.0-00010101000000-000000000000

require (
	github.com/klauspost/cpuid/v2 v2.0.12 // indirect
	github.com/zeebo/blake3 v0.2.4 // indirect
)
