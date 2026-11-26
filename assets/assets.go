package assets

import _ "embed"

// LoadingMP3 contient la piste audio de chargement embarquée dans astral.exe.
//
//go:embed loading.mp3
var LoadingMP3 []byte
