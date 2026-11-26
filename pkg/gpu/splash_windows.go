//go:build windows

package gpu

import (
	"bytes"
	_ "embed"
	"image"
	"image/jpeg"
	"math"
	"sync"
	"sync/atomic"
)

//go:embed astral_logo.jpg
var astralLogoJPG []byte

var (
	bootProgressMilli atomic.Int32 // 0..1000 (0.0% .. 100.0%)
	bootDSReads       atomic.Uint64

	splashOnce    sync.Once
	splashLogoPix []byte
	splashLogoW   int
	splashLogoH   int
)

// ReportBootProgress met à jour la progression de chargement affichée sur l'écran splash (0.0 .. 1.0).
// La progression est monotone croissante pour éviter tout retour en arrière visuel.
func ReportBootProgress(pct float32) {
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	want := int32(pct * 1000)
	for {
		cur := bootProgressMilli.Load()
		if want <= cur {
			return
		}
		if bootProgressMilli.CompareAndSwap(cur, want) {
			return
		}
	}
}

// ReportDirectStorageRead fait progresser la barre de chargement entre 5 % et 82 %
// au fil des lectures DirectStorage du démarrage (~9 500 requêtes).
func ReportDirectStorageRead(totalReads uint64) {
	bootDSReads.Store(totalReads)
	// 0 -> 9500 lectures mappé sur [0.05 .. 0.82]
	p := 0.05 + (float32(totalReads)/9500.0)*0.77
	if p > 0.82 {
		p = 0.82
	}
	ReportBootProgress(p)
}

func initSplashLogo() {
	splashOnce.Do(func() {
		img, err := jpeg.Decode(bytes.NewReader(astralLogoJPG))
		if err != nil || img == nil {
			return
		}
		b := img.Bounds()
		w, h := b.Dx(), b.Dy()
		rgba := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				off := y*rgba.Stride + x*4
				rgba.Pix[off+0] = byte(r >> 8)
				rgba.Pix[off+1] = byte(g >> 8)
				rgba.Pix[off+2] = byte(bl >> 8)
				rgba.Pix[off+3] = 255
			}
		}
		splashLogoPix = rgba.Pix
		splashLogoW = w
		splashLogoH = h
	})
}

// renderSplashRGBA génère une trame RGBA8 (w x h) contenant le logo GTA VI Astral centré
// sur fond noir pur et une barre de progression néon synthwave animée.
func (h *HostD3D12) renderSplashRGBA() []byte {
	initSplashLogo()
	w, ht := int(h.Width), int(h.Height)
	if w == 0 || ht == 0 {
		return nil
	}
	need := w * ht * 4
	if len(h.splashBuf) != need {
		h.splashBuf = make([]byte, need)
		// Fond noir opaque + logo copié une seule fois (seule la zone barre est redessinée par trame)
		for i := 3; i < need; i += 4 {
			h.splashBuf[i] = 255
		}
		if len(splashLogoPix) > 0 && splashLogoW > 0 && splashLogoH > 0 {
			dstX0 := (w - splashLogoW) / 2
			dstY0 := (ht-splashLogoH)/2 - 34
			if dstY0 < 16 {
				dstY0 = 16
			}
			for y := 0; y < splashLogoH; y++ {
				dy := dstY0 + y
				if dy < 0 || dy >= ht {
					continue
				}
				for x := 0; x < splashLogoW; x++ {
					dx := dstX0 + x
					if dx < 0 || dx >= w {
						continue
					}
					sOff := (y*splashLogoW + x) * 4
					dOff := (dy*w + dx) * 4
					h.splashBuf[dOff+0] = splashLogoPix[sOff+0]
					h.splashBuf[dOff+1] = splashLogoPix[sOff+1]
					h.splashBuf[dOff+2] = splashLogoPix[sOff+2]
					h.splashBuf[dOff+3] = 255
				}
			}
		}
	}

	h.splashFrame++
	// Progression cible lissée (avance doucement jusqu'à 97 % pendant la compilation finale)
	target := float32(bootProgressMilli.Load()) / 1000.0
	if target < 0.04 {
		target = 0.04
	}
	if target >= 0.88 && target < 0.97 {
		// Progression douce pendant les dispatches Compute / compilation shaders
		extra := float32(h.splashFrame%600) * 0.00025
		if target+extra < 0.97 {
			target += extra
		}
	}
	if h.splashPct < target {
		step := (target - h.splashPct) * 0.12
		if step < 0.002 {
			step = 0.002
		}
		h.splashPct += step
		if h.splashPct > target {
			h.splashPct = target
		}
	}

	// Géométrie de la barre de progression néon sous le logo
	barW := 560
	if barW > w-80 {
		barW = w - 80
	}
	barH := 12
	barX := (w - barW) / 2
	barY := (ht+splashLogoH)/2 - 12
	if barY+barH+16 >= ht {
		barY = ht - barH - 28
	}
	fillW := int(float32(barW-4) * h.splashPct)
	sweepX := int((h.splashFrame * 9) % uint64(barW+160)) - 80

	// Dessin du cadre + piste + remplissage dégradé synthwave (violet -> magenta -> corail) + reflet animé
	for y := barY - 2; y <= barY+barH+1; y++ {
		if y < 0 || y >= ht {
			continue
		}
		for x := barX - 2; x <= barX+barW+1; x++ {
			if x < 0 || x >= w {
				continue
			}
			dOff := (y*w + x) * 4
			// Bordure extérieure néon violet doux
			isBorder := x < barX || x >= barX+barW || y < barY || y >= barY+barH
			if isBorder {
				h.splashBuf[dOff+0] = 88
				h.splashBuf[dOff+1] = 42
				h.splashBuf[dOff+2] = 138
				h.splashBuf[dOff+3] = 255
				continue
			}
			lx := x - barX - 2
			ly := y - barY - 2
			insideTrack := lx >= 0 && lx < barW-4 && ly >= 0 && ly < barH-4
			if insideTrack && lx < fillW {
				t := float32(lx) / float32(barW-4)
				var r, g, bl float32
				if t < 0.55 {
					u := t / 0.55
					// #9D4EDD (157, 78, 221) -> #FF2A85 (255, 42, 133)
					r = 157 + u*(255-157)
					g = 78 + u*(42-78)
					bl = 221 + u*(133-221)
				} else {
					u := (t - 0.55) / 0.45
					// #FF2A85 (255, 42, 133) -> #FF8C42 (255, 140, 66)
					r = 255
					g = 42 + u*(140-42)
					bl = 133 + u*(66-133)
				}
				// Reflet lumineux glissant le long de la barre
				dist := float64(lx - sweepX)
				glow := float32(math.Exp(-(dist * dist) / 1400.0))
				r += glow * 80
				g += glow * 80
				bl += glow * 90
				h.splashBuf[dOff+0] = clampByte(r)
				h.splashBuf[dOff+1] = clampByte(g)
				h.splashBuf[dOff+2] = clampByte(bl)
				h.splashBuf[dOff+3] = 255
			} else {
				// Piste sombre
				h.splashBuf[dOff+0] = 20
				h.splashBuf[dOff+1] = 16
				h.splashBuf[dOff+2] = 32
				h.splashBuf[dOff+3] = 255
			}
		}
	}
	// Bouton audio synthwave en haut à droite (activer / couper loading.mp3)
	drawAudioButton(h.splashBuf, w, ht, IsLoadingAudioEnabled())

	return h.splashBuf
}

const (
	audioBtnMarginRight = 42
	audioBtnCenterY     = 38
	audioBtnRadius      = 22.0
)

// IsPointInAudioButton vérifie si le point (bx, by) en coordonnées backbuffer (w x h)
// se trouve sur le bouton de coupure du son en haut à droite.
func IsPointInAudioButton(bx, by, w, h int) bool {
	if w <= 0 || h <= 0 {
		return false
	}
	cx := w - audioBtnMarginRight
	cy := audioBtnCenterY
	dx := bx - cx
	dy := by - cy
	return dx*dx+dy*dy <= 28*28
}

func blendPixel(buf []byte, off int, r, g, b, alpha float32) {
	if alpha <= 0 {
		return
	}
	if alpha >= 1 {
		buf[off+0] = clampByte(r)
		buf[off+1] = clampByte(g)
		buf[off+2] = clampByte(b)
		buf[off+3] = 255
		return
	}
	inv := 1.0 - alpha
	buf[off+0] = clampByte(float32(buf[off+0])*inv + r*alpha)
	buf[off+1] = clampByte(float32(buf[off+1])*inv + g*alpha)
	buf[off+2] = clampByte(float32(buf[off+2])*inv + b*alpha)
	buf[off+3] = 255
}

func drawAudioButton(buf []byte, w, ht int, soundOn bool) {
	cx := w - audioBtnMarginRight
	cy := audioBtnCenterY
	box := 28

	// 1. Efface la zone du bouton sur fond noir pur puis dessine le badge circulaire néon
	for dy := -box; dy <= box; dy++ {
		y := cy + dy
		if y < 0 || y >= ht {
			continue
		}
		for dx := -box; dx <= box; dx++ {
			x := cx + dx
			if x < 0 || x >= w {
				continue
			}
			off := (y*w + x) * 4
			buf[off+0] = 0
			buf[off+1] = 0
			buf[off+2] = 0
			buf[off+3] = 255

			dist := float32(math.Hypot(float64(dx), float64(dy)))
			if dist > audioBtnRadius+4 {
				continue
			}

			// Halo extérieur discret lorsque le son est actif
			if soundOn && dist > audioBtnRadius {
				glow := (1.0 - (dist-audioBtnRadius)/4.0) * 0.35
				blendPixel(buf, off, 255, 42, 133, glow)
				continue
			}

			// Anneau bordure dégradé synthwave (2 px)
			if dist >= audioBtnRadius-2.2 && dist <= audioBtnRadius {
				edgeAlpha := float32(1.0)
				if dist > audioBtnRadius-1.0 {
					edgeAlpha = audioBtnRadius - dist
				}
				if soundOn {
					t := (float32(dx+dy)/(2.0*audioBtnRadius) + 0.5)
					if t < 0 {
						t = 0
					} else if t > 1 {
						t = 1
					}
					// #9D4EDD -> #FF2A85 -> #FF8C42
					var br, bg, bb float32
					if t < 0.5 {
						u := t * 2
						br = 157 + u*(255-157)
						bg = 78 + u*(42-78)
						bb = 221 + u*(133-221)
					} else {
						u := (t - 0.5) * 2
						br = 255
						bg = 42 + u*(140-42)
						bb = 133 + u*(66-133)
					}
					blendPixel(buf, off, br, bg, bb, edgeAlpha)
				} else {
					blendPixel(buf, off, 96, 80, 118, edgeAlpha)
				}
				continue
			}

			// Fond sombre du badge
			if dist < audioBtnRadius-2.0 {
				if soundOn {
					blendPixel(buf, off, 28, 18, 46, 1.0)
				} else {
					blendPixel(buf, off, 20, 16, 28, 1.0)
				}
			}

			// 2. Glyphe haut-parleur centré-gauche
			lx := float32(dx)
			ly := float32(dy)
			var sr, sg, sb float32 = 246, 238, 255
			if !soundOn {
				sr, sg, sb = 148, 136, 166
			}
			inBody := lx >= -10 && lx <= -5.5 && ly >= -3.8 && ly <= 3.8
			inCone := false
			if lx > -5.5 && lx <= 0.5 {
				u := (lx + 5.5) / 6.0
				halfH := 3.8 + u*5.2
				if ly >= -halfH && ly <= halfH {
					inCone = true
				}
			}
			if inBody || inCone {
				blendPixel(buf, off, sr, sg, sb, 1.0)
				continue
			}

			// 3. Ondes sonores (son actif) ou croix corail-rouge (son coupé)
			if soundOn {
				wx := lx - 1.0
				wy := ly
				if wx > 0.5 {
					rWave := float32(math.Hypot(float64(wx), float64(wy)))
					ang := math.Abs(math.Atan2(float64(wy), float64(wx)))
					if ang <= 0.78 { // ~45 deg
						if d1 := float32(math.Abs(float64(rWave - 5.2))); d1 < 1.25 {
							a := float32(1.0)
							if d1 > 0.6 {
								a = (1.25 - d1) / 0.65
							}
							blendPixel(buf, off, 255, 78, 162, a)
						} else if d2 := float32(math.Abs(float64(rWave - 9.2))); d2 < 1.25 {
							a := float32(1.0)
							if d2 > 0.6 {
								a = (1.25 - d2) / 0.65
							}
							blendPixel(buf, off, 255, 140, 66, a)
						}
					}
				}
			} else {
				// Croix "Muet" à droite du haut-parleur
				if lx >= 3.0 && lx <= 11.0 && ly >= -4.2 && ly <= 4.2 {
					ux := lx - 7.0
					dA := float32(math.Abs(float64(ly - ux)))
					dB := float32(math.Abs(float64(ly + ux)))
					dMin := dA
					if dB < dMin {
						dMin = dB
					}
					if dMin < 1.35 {
						a := float32(1.0)
						if dMin > 0.75 {
							a = (1.35 - dMin) / 0.6
						}
						blendPixel(buf, off, 255, 77, 109, a)
					}
				}
			}
		}
	}
}

func clampByte(v float32) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
