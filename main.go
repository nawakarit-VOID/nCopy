// seqcopy - โปรแกรมคัดลอกไฟล์ทีละไฟล์เรียงตามตัวอักษร (คล้าย TeraCopy)
// เขียนด้วย Go + Fyne สำหรับ Linux
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// ---------- โครงสร้างข้อมูลงานคัดลอก ----------

type jobStatus int

const (
	statusWaiting jobStatus = iota
	statusCopying
	statusDone
	statusSkipped
	statusError
)

type copyJob struct {
	SrcPath string // path ต้นฉบับเต็ม
	RelPath string // path สัมพัทธ์ (ไว้สร้างโครงสร้างโฟลเดอร์ปลายทาง)
	Size    int64
	Status  jobStatus
	Err     error
}

func (j jobStatus) String() string {
	switch j {
	case statusWaiting:
		return "รอคิว"
	case statusCopying:
		return "กำลังคัดลอก"
	case statusDone:
		return "เสร็จแล้ว"
	case statusSkipped:
		return "ข้าม"
	case statusError:
		return "ผิดพลาด"
	}
	return ""
}

type overwritePolicy int

const (
	overwriteAlways overwritePolicy = iota
	overwriteAsk
	overwriteSkip
	overwriteRename
)

func (o overwritePolicy) String() string {
	switch o {
	case overwriteAlways:
		return "เขียนทับเสมอ (Overwrite)"
	case overwriteAsk:
		return "ถามก่อนเขียนทับ (Ask)"
	case overwriteSkip:
		return "ข้ามเมื่อเจอไฟล์ซ้ำ (Skip)"
	case overwriteRename:
		return "เปลี่ยนชื่ออัตโนมัติ (Rename)"
	}
	return ""
}

// ---------- ตัวควบคุมการทำงาน (pause/resume/cancel) ----------

type controller struct {
	mu        sync.Mutex
	cond      *sync.Cond
	paused    bool
	cancelled bool
}

func newController() *controller {
	c := &controller{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *controller) togglePause() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = !c.paused
	c.cond.Broadcast()
	return c.paused
}

func (c *controller) cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = true
	c.paused = false
	c.cond.Broadcast()
}

func (c *controller) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = false
	c.cancelled = false
}

// waitIfPaused บล็อกถ้ากำลังพัก คืนค่า true ถ้าถูกยกเลิก
func (c *controller) waitIfPaused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.paused && !c.cancelled {
		c.cond.Wait()
	}
	return c.cancelled
}

func (c *controller) isCancelled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelled
}

// ---------- แอปหลัก ----------

type app_ struct {
	fyneApp fyne.App
	win     fyne.Window

	sources []string // ไฟล์/โฟลเดอร์ต้นฉบับที่ผู้ใช้เลือก
	destDir string
	policy  overwritePolicy

	jobs []*copyJob

	ctrl    *controller
	running bool

	// UI
	sourceList   *widget.List
	destLabel    *widget.Label
	fileList     *widget.List
	currentLabel *widget.Label
	fileProgress *widget.ProgressBar
	overallProg  *widget.ProgressBar
	overallLabel *widget.Label
	speedLabel   *widget.Label
	btnStart     *widget.Button
	btnPause     *widget.Button
	btnCancel    *widget.Button
}

func main() {
	a := app.New()
	w := a.NewWindow("nCopy - คัดลอกไฟล์เรียงตามตัวอักษร")
	w.Resize(fyne.NewSize(720, 640))

	ap := &app_{fyneApp: a, win: w, ctrl: newController()}
	w.SetContent(ap.buildUI())
	w.ShowAndRun()
}

func (a *app_) buildUI() fyne.CanvasObject {
	// --- ส่วนเลือกต้นฉบับ ---
	a.sourceList = widget.NewList(
		func() int { return len(a.sources) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			o.(*widget.Label).SetText(a.sources[i])
		},
	)
	a.sourceList.Resize(fyne.NewSize(680, 100))

	btnAddFiles := widget.NewButtonWithIcon("เลือกไฟล์...", nil, func() {
		fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
			if err != nil || rc == nil {
				return
			}
			defer rc.Close()
			a.sources = append(a.sources, rc.URI().Path())
			a.sourceList.Refresh()
			a.rebuildQueue()
		}, a.win)
		fd.Show()
	})

	btnAddFolder := widget.NewButtonWithIcon("เลือกโฟลเดอร์...", nil, func() {
		fd := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
			if err != nil || u == nil {
				return
			}
			a.sources = append(a.sources, u.Path())
			a.sourceList.Refresh()
			a.rebuildQueue()
		}, a.win)
		fd.Show()
	})

	btnClear := widget.NewButtonWithIcon("ล้างรายการ", nil, func() {
		a.sources = nil
		a.jobs = nil
		a.sourceList.Refresh()
		a.fileList.Refresh()
		a.updateOverall(0, 0, 0, 0)
	})

	sourceButtons := container.NewHBox(btnAddFiles, btnAddFolder, btnClear)

	// --- ส่วนเลือกปลายทาง & ตัวเลือกไฟล์ซ้ำ ---
	a.destLabel = widget.NewLabel("(ยังไม่ได้เลือกโฟลเดอร์ปลายทาง)")
	btnDest := widget.NewButtonWithIcon("เลือกปลายทาง...", nil, func() {
		fd := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
			if err != nil || u == nil {
				return
			}
			a.destDir = u.Path()
			a.destLabel.SetText(a.destDir)
		}, a.win)
		fd.Show()
	})
	destRow := container.NewBorder(nil, nil, nil, btnDest, a.destLabel)

	policySelect := widget.NewSelect([]string{
		overwriteAlways.String(),
		overwriteAsk.String(),
		overwriteSkip.String(),
		overwriteRename.String(),
	}, func(selected string) {
		switch selected {
		case overwriteAlways.String():
			a.policy = overwriteAlways
		case overwriteAsk.String():
			a.policy = overwriteAsk
		case overwriteSkip.String():
			a.policy = overwriteSkip
		case overwriteRename.String():
			a.policy = overwriteRename
		}
	})
	policySelect.SetSelected(overwriteAlways.String())
	policyRow := container.NewHBox(
		widget.NewLabelWithStyle("กรณีพบไฟล์ซ้ำ:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		policySelect,
	)

	// --- รายการคิวไฟล์ ---
	a.fileList = widget.NewList(
		func() int { return len(a.jobs) },
		func() fyne.CanvasObject {
			nameLbl := widget.NewLabel("")
			statusLbl := widget.NewLabel("")
			statusLbl.Alignment = fyne.TextAlignTrailing
			return container.NewHBox(nameLbl, statusLbl)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			row := o.(*fyne.Container)
			nameLbl := row.Objects[0].(*widget.Label)
			statusLbl := row.Objects[1].(*widget.Label)
			j := a.jobs[i]
			nameLbl.SetText(j.RelPath)
			statusLbl.SetText(j.Status.String())
		},
	)

	// --- ส่วนความคืบหน้า ---
	a.currentLabel = widget.NewLabel("ยังไม่เริ่มคัดลอก")
	a.fileProgress = widget.NewProgressBar()
	a.overallProg = widget.NewProgressBar()
	a.overallLabel = widget.NewLabel("0 / 0 ไฟล์  (0 B / 0 B)")
	a.speedLabel = widget.NewLabel("")

	a.btnStart = widget.NewButtonWithIcon("เริ่มคัดลอก", nil, a.onStart)
	a.btnPause = widget.NewButtonWithIcon("หยุดชั่วคราว", nil, a.onPauseResume)
	a.btnCancel = widget.NewButtonWithIcon("ยกเลิก", nil, a.onCancel)
	a.btnPause.Disable()
	a.btnCancel.Disable()

	controlRow := container.NewHBox(a.btnStart, a.btnPause, a.btnCancel)

	progressBox := container.NewVBox(
		a.currentLabel,
		a.fileProgress,
		a.overallLabel,
		a.overallProg,
		a.speedLabel,
		controlRow,
	)

	top := container.NewVBox(
		widget.NewLabelWithStyle("ต้นฉบับ", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewVScroll(a.sourceList),
		sourceButtons,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("ปลายทาง", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		destRow,
		policyRow,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("คิวไฟล์ (เรียงตามตัวอักษร)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	)

	center := container.NewVScroll(a.fileList)
	center.SetMinSize(fyne.NewSize(680, 220))

	return container.NewBorder(top, progressBox, nil, nil, center)
}

// rebuildQueue สแกนไฟล์/โฟลเดอร์ที่เลือกทั้งหมด แล้วเรียงตามตัวอักษร
func (a *app_) rebuildQueue() {
	var jobs []*copyJob

	for _, src := range a.sources {
		info, err := os.Stat(src)
		if err != nil {
			continue
		}
		if info.IsDir() {
			base := filepath.Base(src)
			filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
				if err != nil || fi.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(src, p)
				rel = filepath.Join(base, rel)
				jobs = append(jobs, &copyJob{SrcPath: p, RelPath: rel, Size: fi.Size(), Status: statusWaiting})
				return nil
			})
		} else {
			jobs = append(jobs, &copyJob{SrcPath: src, RelPath: filepath.Base(src), Size: info.Size(), Status: statusWaiting})
		}
	}

	sort.Slice(jobs, func(i, j int) bool {
		return strings.ToLower(jobs[i].RelPath) < strings.ToLower(jobs[j].RelPath)
	})

	a.jobs = jobs
	a.fileList.Refresh()
	var total int64
	for _, j := range jobs {
		total += j.Size
	}
	a.updateOverall(0, len(jobs), 0, total)
}

func (a *app_) updateOverall(doneCount, totalCount int, doneBytes, totalBytes int64) {
	fyne.Do(func() {
		a.overallLabel.SetText(fmt.Sprintf("%d / %d ไฟล์  (%s / %s)",
			doneCount, totalCount, humanSize(doneBytes), humanSize(totalBytes)))
		if totalBytes > 0 {
			a.overallProg.SetValue(float64(doneBytes) / float64(totalBytes))
		} else {
			a.overallProg.SetValue(0)
		}
	})
}

func humanSize(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// ---------- การเริ่มคัดลอก ----------

func (a *app_) onStart() {
	if a.running {
		return
	}
	if len(a.jobs) == 0 {
		dialog.ShowInformation("ไม่มีไฟล์", "กรุณาเลือกไฟล์หรือโฟลเดอร์ต้นฉบับก่อน", a.win)
		return
	}
	if a.destDir == "" {
		dialog.ShowInformation("ไม่มีปลายทาง", "กรุณาเลือกโฟลเดอร์ปลายทางก่อน", a.win)
		return
	}

	// รีเซ็ตสถานะงานทั้งหมด (เผื่อคัดลอกซ้ำ)
	for _, j := range a.jobs {
		j.Status = statusWaiting
		j.Err = nil
	}
	a.ctrl.reset()
	a.running = true
	a.btnStart.Disable()
	a.btnPause.Enable()
	a.btnPause.SetText("หยุดชั่วคราว")
	a.btnCancel.Enable()

	go a.runCopy()
}

func (a *app_) onPauseResume() {
	if !a.running {
		return
	}
	paused := a.ctrl.togglePause()
	if paused {
		a.btnPause.SetText("ทำต่อ")
	} else {
		a.btnPause.SetText("หยุดชั่วคราว")
	}
}

func (a *app_) onCancel() {
	if !a.running {
		return
	}
	a.ctrl.cancel()
}

// runCopy คัดลอกไฟล์ทีละไฟล์ตามลำดับใน a.jobs (ซึ่งเรียงตามตัวอักษรแล้ว)
func (a *app_) runCopy() {
	var totalBytes, doneBytes int64
	for _, j := range a.jobs {
		totalBytes += j.Size
	}

	doneCount := 0
	for idx, j := range a.jobs {
		if a.ctrl.isCancelled() {
			j.Status = statusSkipped
			continue
		}

		j.Status = statusCopying
		a.safeRefreshFileList()
		relPath := j.RelPath
		fyne.Do(func() {
			a.currentLabel.SetText(fmt.Sprintf("กำลังคัดลอก: %s", relPath))
			a.fileProgress.SetValue(0)
		})

		destPath := filepath.Join(a.destDir, j.RelPath)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			j.Status = statusError
			j.Err = err
			continue
		}

		finalDstPath, action := a.resolveDestination(destPath, j.RelPath)
		if action == "skip" {
			j.Status = statusSkipped
			a.safeRefreshFileList()
			continue
		}

		start := time.Now()
		copiedInFile, err := a.copyOneFile(j.SrcPath, finalDstPath, j.Size, func(copied int64) {
			if j.Size > 0 {
				val := float64(copied) / float64(j.Size)
				fyne.Do(func() {
					a.fileProgress.SetValue(val)
				})
			}
			elapsed := time.Since(start).Seconds()
			if elapsed > 0.2 {
				speed := float64(copied) / elapsed
				speedStr := fmt.Sprintf("ความเร็ว: %s/วินาที", humanSize(int64(speed)))
				fyne.Do(func() {
					a.speedLabel.SetText(speedStr)
				})
			}
			a.updateOverall(doneCount, len(a.jobs), doneBytes+copied, totalBytes)
		})

		if err != nil {
			if err == errCancelled {
				j.Status = statusSkipped
			} else {
				j.Status = statusError
				j.Err = err
			}
		} else {
			j.Status = statusDone
			doneBytes += copiedInFile
			doneCount++
		}

		a.safeRefreshFileList()
		a.updateOverall(doneCount, len(a.jobs), doneBytes, totalBytes)

		if a.ctrl.isCancelled() {
			// ทำเครื่องหมายไฟล์ที่เหลือว่าถูกข้าม
			for _, rest := range a.jobs[idx+1:] {
				rest.Status = statusSkipped
			}
			a.safeRefreshFileList()
			break
		}
	}

	a.running = false
	fyne.Do(func() {
		a.currentLabel.SetText("เสร็จสิ้น (หรือถูกยกเลิก)")
		a.btnStart.Enable()
		a.btnPause.Disable()
		a.btnCancel.Disable()
	})
}

var errCancelled = fmt.Errorf("ยกเลิกโดยผู้ใช้")

const copyBufSize = 1024 * 1024 // 1MB ต่อ chunk

// copyOneFile คัดลอกไฟล์เดียวเป็น chunk พร้อมเช็ค pause/cancel ระหว่างทาง
func (a *app_) copyOneFile(src, dst string, size int64, onProgress func(copied int64)) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	buf := make([]byte, copyBufSize)
	var copied int64
	for {
		if a.ctrl.waitIfPaused() {
			return copied, errCancelled
		}
		if a.ctrl.isCancelled() {
			return copied, errCancelled
		}

		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return copied, werr
			}
			copied += int64(n)
			if onProgress != nil {
				onProgress(copied)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return copied, rerr
		}
	}
	return copied, nil
}

// resolveDestination ตรวจสอบไฟล์ซ้ำตามนโยบาย (policy) ที่เลือก
// คืนค่า (path ปลายทางที่จะใช้, แอคชัน "copy" หรือ "skip")
func (a *app_) resolveDestination(destPath, relPath string) (string, string) {
	if _, err := os.Stat(destPath); os.IsNotExist(err) {
		return destPath, "copy"
	}

	switch a.policy {
	case overwriteAlways:
		return destPath, "copy"

	case overwriteSkip:
		return destPath, "skip"

	case overwriteRename:
		ext := filepath.Ext(destPath)
		base := strings.TrimSuffix(destPath, ext)
		counter := 1
		for {
			newName := fmt.Sprintf("%s (%d)%s", base, counter, ext)
			if _, err := os.Stat(newName); os.IsNotExist(err) {
				return newName, "copy"
			}
			counter++
		}

	case overwriteAsk:
		ch := make(chan string)
		fyne.Do(func() {
			d := dialog.NewConfirm(
				"พบไฟล์ซ้ำ",
				fmt.Sprintf("ไฟล์ปลายทาง %s มีอยู่แล้ว\nคุณต้องการเขียนทับหรือไม่?", relPath),
				func(overwrite bool) {
					if overwrite {
						ch <- "copy"
					} else {
						ch <- "skip"
					}
				},
				a.win,
			)
			d.SetConfirmText("เขียนทับ (Overwrite)")
			d.SetDismissText("ข้ามไฟล์นี้ (Skip)")
			d.Show()
		})
		action := <-ch
		return destPath, action
	}

	return destPath, "copy"
}

// safeRefreshFileList รีเฟรช list widget (เรียกจาก goroutine พื้นหลัง)
func (a *app_) safeRefreshFileList() {
	fyne.Do(func() {
		if a.fileList != nil {
			a.fileList.Refresh()
		}
	})
}

// เก็บ import storage ไว้เผื่อขยายการรองรับ URI แบบอื่นในอนาคต
//var _ = storage.NewFileReader
