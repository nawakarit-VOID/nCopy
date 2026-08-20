// seqcopy - โปรแกรมคัดลอกไฟล์ทีละไฟล์เรียงตามตัวอักษร (คล้าย TeraCopy)
// เขียนด้วย Go + Fyne สำหรับ Linux
package main

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
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
	statusVerifying
	statusRetrying
	statusDone
	statusSkipped
	statusError
	statusVerifyFailed
)

type copyJob struct {
	SrcPath  string // path ต้นฉบับเต็ม
	RelPath  string // path สัมพัทธ์ (ไว้สร้างโครงสร้างโฟลเดอร์ปลายทาง)
	Size     int64
	Status   jobStatus
	Err      error
	Retries  int // จำนวนครั้งที่ Retry แล้ว
}

func (j jobStatus) String() string {
	switch j {
	case statusWaiting:
		return "รอคิว"
	case statusCopying:
		return "กำลังคัดลอก"
	case statusVerifying:
		return "กำลังตรวจ Hash"
	case statusRetrying:
		return "กำลัง Retry..."
	case statusDone:
		return "เสร็จแล้ว"
	case statusSkipped:
		return "ข้าม"
	case statusError:
		return "ผิดพลาด"
	case statusVerifyFailed:
		return "Checksum ล้มเหลว!"
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

type verifyMode int

const (
	verifyNone verifyMode = iota
	verifyMD5
	verifySHA256
)

func (v verifyMode) String() string {
	switch v {
	case verifyNone:
		return "ไม่ตรวจสอบ (None)"
	case verifyMD5:
		return "ตรวจสอบ MD5 Hash"
	case verifySHA256:
		return "ตรวจสอบ SHA256 Hash"
	}
	return ""
}

type queueSortOrder int

const (
	sortNameAsc queueSortOrder = iota
	sortNameDesc
	sortSizeAsc
	sortSizeDesc
)

func (q queueSortOrder) String() string {
	switch q {
	case sortNameAsc:
		return "เรียงตามชื่อ (A-Z)"
	case sortNameDesc:
		return "เรียงตามชื่อ (Z-A)"
	case sortSizeAsc:
		return "เรียงตามขนาด (เล็ก -> ใหญ่)"
	case sortSizeDesc:
		return "เรียงตามขนาด (ใหญ่ -> เล็ก)"
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
	destDir          string
	policy           overwritePolicy
	verify           verifyMode
	sortOrder        queueSortOrder
	preserveMetadata bool
	maxRetry         int // จำนวนครั้ง Retry สูงสุด (0 = ไม่ retry)

	errorLog []string   // สะสม error ตลอด session
	jobs     []*copyJob

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
	etaLabel     *widget.Label
	btnStart     *widget.Button
	btnPause     *widget.Button
	btnCancel    *widget.Button
}

func main() {
	a := app.New()
	w := a.NewWindow("nCopy - คัดลอกไฟล์เรียงตามตัวอักษร")
	w.Resize(fyne.NewSize(720, 640))

	ap := &app_{fyneApp: a, win: w, ctrl: newController(), preserveMetadata: true, maxRetry: 3}

	// รองรับ Drag & Drop ลากไฟล์/โฟลเดอร์มาวางในหน้าต่างโปรแกรม
	w.SetOnDropped(func(pos fyne.Position, uris []fyne.URI) {
		added := false
		for _, u := range uris {
			if u.Scheme() == "file" || u.Scheme() == "" {
				path := u.Path()
				if path != "" {
					ap.sources = append(ap.sources, path)
					added = true
				}
			}
		}
		if added {
			ap.sourceList.Refresh()
			ap.rebuildQueue()
		}
	})

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

	verifySelect := widget.NewSelect([]string{
		verifyNone.String(),
		verifyMD5.String(),
		verifySHA256.String(),
	}, func(selected string) {
		switch selected {
		case verifyNone.String():
			a.verify = verifyNone
		case verifyMD5.String():
			a.verify = verifyMD5
		case verifySHA256.String():
			a.verify = verifySHA256
		}
	})
	verifySelect.SetSelected(verifyNone.String())

	sortSelect := widget.NewSelect([]string{
		sortNameAsc.String(),
		sortNameDesc.String(),
		sortSizeAsc.String(),
		sortSizeDesc.String(),
	}, func(selected string) {
		switch selected {
		case sortNameAsc.String():
			a.sortOrder = sortNameAsc
		case sortNameDesc.String():
			a.sortOrder = sortNameDesc
		case sortSizeAsc.String():
			a.sortOrder = sortSizeAsc
		case sortSizeDesc.String():
			a.sortOrder = sortSizeDesc
		}
		if len(a.jobs) > 0 {
			a.applySort(a.jobs)
			a.fileList.Refresh()
		}
	})
	sortSelect.SetSelected(sortNameAsc.String())

	preserveCheck := widget.NewCheck("คงค่า Metadata (เวลา/สิทธิ์ไฟล์)", func(checked bool) {
		a.preserveMetadata = checked
	})
	preserveCheck.SetChecked(a.preserveMetadata)

	retrySelect := widget.NewSelect([]string{"ไม่ Retry (0)", "1 ครั้ง", "3 ครั้ง", "5 ครั้ง", "10 ครั้ง"}, func(selected string) {
		switch selected {
		case "ไม่ Retry (0)":
			a.maxRetry = 0
		case "1 ครั้ง":
			a.maxRetry = 1
		case "3 ครั้ง":
			a.maxRetry = 3
		case "5 ครั้ง":
			a.maxRetry = 5
		case "10 ครั้ง":
			a.maxRetry = 10
		}
	})
	retrySelect.SetSelected("3 ครั้ง")

	optionsRow := container.NewHBox(
		widget.NewLabelWithStyle("กรณีไฟล์ซ้ำ:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		policySelect,
		widget.NewLabelWithStyle("  เรียงคิว:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		sortSelect,
		widget.NewLabelWithStyle("  ตรวจสอบ:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		verifySelect,
		preserveCheck,
	)

	retryRow := container.NewHBox(
		widget.NewLabelWithStyle("Retry เมื่อเกิด Error:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		retrySelect,
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
	a.etaLabel = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

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
		container.NewHBox(a.speedLabel, widget.NewLabel("  "), a.etaLabel),
		controlRow,
	)

	top := container.NewVBox(
		widget.NewLabelWithStyle("ต้นฉบับ", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewVScroll(a.sourceList),
		sourceButtons,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("ปลายทาง", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		destRow,
		optionsRow,
		retryRow,
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

	a.applySort(jobs)

	a.jobs = jobs
	a.fileList.Refresh()
	var total int64
	for _, j := range jobs {
		total += j.Size
	}
	a.updateOverall(0, len(jobs), 0, total)
}

func (a *app_) applySort(jobs []*copyJob) {
	sort.Slice(jobs, func(i, j int) bool {
		switch a.sortOrder {
		case sortNameAsc:
			return strings.ToLower(jobs[i].RelPath) < strings.ToLower(jobs[j].RelPath)
		case sortNameDesc:
			return strings.ToLower(jobs[i].RelPath) > strings.ToLower(jobs[j].RelPath)
		case sortSizeAsc:
			if jobs[i].Size == jobs[j].Size {
				return strings.ToLower(jobs[i].RelPath) < strings.ToLower(jobs[j].RelPath)
			}
			return jobs[i].Size < jobs[j].Size
		case sortSizeDesc:
			if jobs[i].Size == jobs[j].Size {
				return strings.ToLower(jobs[i].RelPath) < strings.ToLower(jobs[j].RelPath)
			}
			return jobs[i].Size > jobs[j].Size
		}
		return strings.ToLower(jobs[i].RelPath) < strings.ToLower(jobs[j].RelPath)
	})
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

// formatDuration แปลง seconds เป็น HH:MM:SS หรือ MM:SS
func formatDuration(sec int) string {
	if sec < 0 {
		sec = 0
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
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

// runCopy คัดลอกไฟล์ทีละไฟล์ตามลำดับใน a.jobs พร้อม Auto-Retry และ Error Log
func (a *app_) runCopy() {
	var totalBytes, doneBytes int64
	for _, j := range a.jobs {
		totalBytes += j.Size
	}

	// รีเซ็ต error log ของรอบนี้
	a.errorLog = nil
	sessionStart := time.Now()

	// รีเซ็ต ETA label
	fyne.Do(func() {
		a.etaLabel.SetText("")
		a.speedLabel.SetText("")
	})

	doneCount := 0
	for idx, j := range a.jobs {
		if a.ctrl.isCancelled() {
			j.Status = statusSkipped
			continue
		}

		j.Status = statusCopying
		j.Retries = 0
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
			a.errorLog = append(a.errorLog, fmt.Sprintf("[Error] %s\n  → %s", relPath, err.Error()))
			continue
		}

		finalDstPath, action := a.resolveDestination(j.SrcPath, destPath, j.RelPath)
		if action == "skip" {
			j.Status = statusSkipped
			a.safeRefreshFileList()
			continue
		}

		lastChunkTime := time.Now()
		lastCopied := int64(0)
		var currentSpeed float64 // EMA speed ต่อ chunk (B/s)

		progressFn := func(copied int64) {
			if j.Size > 0 {
				val := float64(copied) / float64(j.Size)
				fyne.Do(func() {
					a.fileProgress.SetValue(val)
				})
			}

			now := time.Now()
			elapsed := now.Sub(lastChunkTime).Seconds()
			if elapsed >= 0.4 {
				bytesDiff := copied - lastCopied
				instSpeed := float64(bytesDiff) / elapsed
				if currentSpeed == 0 {
					currentSpeed = instSpeed
				} else {
					alpha := 0.3
					currentSpeed = alpha*instSpeed + (1-alpha)*currentSpeed
				}

				// --- ETA คำนวณจาก global session speed (แม่นกว่า per-file) ---
				sessionElapsed := now.Sub(sessionStart).Seconds()
				totalDone := doneBytes + copied
				var etaStr string
				if sessionElapsed > 1 && totalDone > 0 {
					sessionSpeed := float64(totalDone) / sessionElapsed // B/s เฉลี่ยทั้ง session
					remBytes := totalBytes - totalDone
					if remBytes > 0 && sessionSpeed > 0 {
						remSec := int(float64(remBytes) / sessionSpeed)
						etaStr = formatDuration(remSec)
					} else if remBytes <= 0 {
						etaStr = "เสร็จแล้ว"
					}
				}

				// elapsed time ของ session
				elapsedStr := formatDuration(int(sessionElapsed))

				speedStr := fmt.Sprintf("ความเร็ว: %s/วินาที", humanSize(int64(currentSpeed)))
				etaDisplay := ""
				if etaStr != "" {
					etaDisplay = fmt.Sprintf("⏱ ผ่านมา %s  |  เหลือ ~%s", elapsedStr, etaStr)
				} else {
					etaDisplay = fmt.Sprintf("⏱ ผ่านมา %s", elapsedStr)
				}
				fyne.Do(func() {
					a.speedLabel.SetText(speedStr)
					a.etaLabel.SetText(etaDisplay)
				})
				lastChunkTime = now
				lastCopied = copied
			}
			a.updateOverall(doneCount, len(a.jobs), doneBytes+copied, totalBytes)
		}

		// ---------- Auto-Retry Loop ----------
		var copiedInFile int64
		var copyErr error
		for attempt := 0; ; attempt++ {
			if attempt > 0 {
				// รอ 1 วินาทีก่อน retry เพื่อให้ lock ไฟล์คลาย
				time.Sleep(time.Second)
				if a.ctrl.isCancelled() {
					copyErr = errCancelled
					break
				}
				j.Status = statusRetrying
				attemptNum := attempt
				fyne.Do(func() {
					a.currentLabel.SetText(fmt.Sprintf("Retry (%d/%d): %s", attemptNum, a.maxRetry, relPath))
					a.fileProgress.SetValue(0)
				})
				a.safeRefreshFileList()
			}

			copiedInFile, copyErr = a.copyOneFile(j.SrcPath, finalDstPath, j.Size, progressFn)

			if copyErr == nil || copyErr == errCancelled {
				break // สำเร็จ หรือถูกยกเลิก ออกจาก retry loop
			}

			j.Retries = attempt + 1
			if attempt >= a.maxRetry {
				break // หมดจำนวน retry แล้ว
			}
		}
		// ----------------------------------

		if copyErr != nil {
			if copyErr == errCancelled {
				j.Status = statusSkipped
			} else {
				j.Status = statusError
				j.Err = copyErr
				errMsg := fmt.Sprintf("[Error] %s (retry %d/%d)\n  → %s",
					relPath, j.Retries, a.maxRetry, copyErr.Error())
				a.errorLog = append(a.errorLog, errMsg)
			}
		} else {
			// ตรวจสอบ Hash หากเปิดใช้งาน
			if a.verify != verifyNone {
				j.Status = statusVerifying
				a.safeRefreshFileList()
				fyne.Do(func() {
					a.currentLabel.SetText(fmt.Sprintf("กำลังตรวจสอบ Hash: %s", relPath))
				})
				vErr := a.verifyHash(j.SrcPath, finalDstPath)
				if vErr != nil {
					j.Status = statusVerifyFailed
					j.Err = vErr
					a.errorLog = append(a.errorLog, fmt.Sprintf("[VerifyFailed] %s\n  → %s", relPath, vErr.Error()))
				} else {
					j.Status = statusDone
					doneBytes += copiedInFile
					doneCount++
				}
			} else {
				j.Status = statusDone
				doneBytes += copiedInFile
				doneCount++
			}
		}

		a.safeRefreshFileList()
		a.updateOverall(doneCount, len(a.jobs), doneBytes, totalBytes)

		if a.ctrl.isCancelled() {
			for _, rest := range a.jobs[idx+1:] {
				rest.Status = statusSkipped
			}
			a.safeRefreshFileList()
			break
		}
	}

	a.running = false
	errSnapshot := a.errorLog // snapshot ก่อนเข้า fyne.Do

	fyne.Do(func() {
		a.currentLabel.SetText("เสร็จสิ้น (หรือถูกยกเลิก)")
		a.btnStart.Enable()
		a.btnPause.Disable()
		a.btnCancel.Disable()

		// แสดง Error Summary หากมี error
		if len(errSnapshot) > 0 {
			summary := fmt.Sprintf("พบ %d ไฟล์ที่เกิดข้อผิดพลาด:\n\n", len(errSnapshot))
			for i, e := range errSnapshot {
				summary += fmt.Sprintf("%d. %s\n\n", i+1, e)
			}
			logLbl := widget.NewLabel(summary)
			logLbl.Wrapping = fyne.TextWrapWord
			scroll := container.NewVScroll(logLbl)
			scroll.SetMinSize(fyne.NewSize(560, 300))
			d := dialog.NewCustom("Error Log - สรุปไฟล์ที่ผิดพลาด", "ปิด", scroll, a.win)
			d.Resize(fyne.NewSize(600, 400))
			d.Show()
		}
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

	srcInfo, err := in.Stat()
	if err == nil && a.preserveMetadata {
		// คัดลอก Permissions (Chmod)
		_ = out.Chmod(srcInfo.Mode())
	}

	buf := make([]byte, copyBufSize)
	var copied int64
	for {
		if a.ctrl.waitIfPaused() || a.ctrl.isCancelled() {
			out.Close()
			os.Remove(dst) // ลบไฟล์ค้างที่ไม่สมบูรณ์เมื่อถูกยกเลิก
			return copied, errCancelled
		}

		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				os.Remove(dst) // ลบไฟล์ที่ไม่สมบูรณ์กรณีเกิด Error ขณะเขียน
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
			out.Close()
			os.Remove(dst) // ลบไฟล์ที่ไม่สมบูรณ์กรณีเกิด Error ขณะอ่าน
			return copied, rerr
		}
	}

	// คัดลอกเวลาแก้ไขล่าสุด (Modification Time / Access Time)
	if srcInfo != nil && a.preserveMetadata {
		_ = out.Close() // ปิดไฟล์ก่อนตั้งเวลา
		_ = os.Chtimes(dst, srcInfo.ModTime(), srcInfo.ModTime())
	}

	return copied, nil
}

// resolveDestination ตรวจสอบไฟล์ซ้ำตามนโยบาย (policy) ที่เลือก
// คืนค่า (path ปลายทางที่จะใช้, แอคชัน "copy" หรือ "skip")
func (a *app_) resolveDestination(srcPath, destPath, relPath string) (string, string) {
	dstInfo, err := os.Stat(destPath)
	if os.IsNotExist(err) {
		return destPath, "copy"
	}

	switch a.policy {
	case overwriteAlways:
		return destPath, "copy"

	case overwriteSkip:
		return destPath, "skip"

	case overwriteRename:
		return generateUniqueName(destPath), "copy"

	case overwriteAsk:
		srcInfo, sErr := os.Stat(srcPath)

		srcSizeStr, srcTimeStr := "ไม่ทราบ", "ไม่ทราบ"
		if sErr == nil {
			srcSizeStr = humanSize(srcInfo.Size())
			srcTimeStr = srcInfo.ModTime().Format("2006-01-02 15:04:05")
		}

		dstSizeStr, dstTimeStr := "ไม่ทราบ", "ไม่ทราบ"
		if err == nil {
			dstSizeStr = humanSize(dstInfo.Size())
			dstTimeStr = dstInfo.ModTime().Format("2006-01-02 15:04:05")
		}

		type result struct {
			action     string
			targetPath string
			applyAll   bool
			chosenPolicy overwritePolicy
		}
		ch := make(chan result)

		fyne.Do(func() {
			msgLabel := widget.NewLabel(fmt.Sprintf("พบไฟล์ปลายทางที่มีชื่อซ้ำกัน:\n%s", relPath))
			msgLabel.TextStyle = fyne.TextStyle{Bold: true}

			srcBox := container.NewVBox(
				widget.NewLabelWithStyle("ต้นทาง (Source)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				widget.NewLabel(fmt.Sprintf("ขนาด: %s", srcSizeStr)),
				widget.NewLabel(fmt.Sprintf("แก้ไขล่าสุด: %s", srcTimeStr)),
			)

			dstBox := container.NewVBox(
				widget.NewLabelWithStyle("ปลายทางเดิม (Destination)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				widget.NewLabel(fmt.Sprintf("ขนาด: %s", dstSizeStr)),
				widget.NewLabel(fmt.Sprintf("แก้ไขล่าสุด: %s", dstTimeStr)),
			)

			comparisonContainer := container.NewGridWithColumns(2, srcBox, dstBox)
			applyAllCheck := widget.NewCheck("นำตัวเลือกนี้ไปใช้กับไฟล์ซ้ำที่เหลือทั้งหมดในรอบนี้ (Apply to all)", nil)

			var d dialog.Dialog
			
			btnOverwrite := widget.NewButton("เขียนทับ (Overwrite)", func() {
				d.Hide()
				ch <- result{
					action:       "copy",
					targetPath:   destPath,
					applyAll:     applyAllCheck.Checked,
					chosenPolicy: overwriteAlways,
				}
			})
			btnOverwrite.Importance = widget.HighImportance

			btnRename := widget.NewButton("เปลี่ยนชื่ออัตโนมัติ (Rename)", func() {
				d.Hide()
				ch <- result{
					action:       "copy",
					targetPath:   generateUniqueName(destPath),
					applyAll:     applyAllCheck.Checked,
					chosenPolicy: overwriteRename,
				}
			})

			btnSkip := widget.NewButton("ข้ามไฟล์นี้ (Skip)", func() {
				d.Hide()
				ch <- result{
					action:       "skip",
					targetPath:   destPath,
					applyAll:     applyAllCheck.Checked,
					chosenPolicy: overwriteSkip,
				}
			})

			btnBox := container.NewHBox(btnOverwrite, btnRename, btnSkip)
			content := container.NewVBox(
				msgLabel,
				widget.NewSeparator(),
				comparisonContainer,
				widget.NewSeparator(),
				applyAllCheck,
				btnBox,
			)

			d = dialog.NewCustomWithoutButtons("พบไฟล์ซ้ำในปลายทาง (File Conflict)", content, a.win)
			d.Show()
		})

		res := <-ch
		if res.applyAll {
			a.policy = res.chosenPolicy
		}
		return res.targetPath, res.action
	}

	return destPath, "copy"
}

func generateUniqueName(destPath string) string {
	ext := filepath.Ext(destPath)
	base := strings.TrimSuffix(destPath, ext)
	counter := 1
	for {
		newName := fmt.Sprintf("%s (%d)%s", base, counter, ext)
		if _, err := os.Stat(newName); os.IsNotExist(err) {
			return newName
		}
		counter++
	}
}

// safeRefreshFileList รีเฟรช list widget (เรียกจาก goroutine พื้นหลัง)
func (a *app_) safeRefreshFileList() {
	fyne.Do(func() {
		if a.fileList != nil {
			a.fileList.Refresh()
		}
	})
}

// verifyHash คำนวณและเปรียบเทียบ Hash ระหว่างต้นทางและปลายทาง
func (a *app_) verifyHash(src, dst string) error {
	srcHash, err := a.computeFileHash(src)
	if err != nil {
		return fmt.Errorf("คำนวณ Hash ต้นทางล้มเหลว: %w", err)
	}

	dstHash, err := a.computeFileHash(dst)
	if err != nil {
		return fmt.Errorf("คำนวณ Hash ปลายทางล้มเหลว: %w", err)
	}

	if srcHash != dstHash {
		return fmt.Errorf("Checksum ไม่ตรงกัน! (ต้นทาง: %s..., ปลายทาง: %s...)",
			truncateHash(srcHash), truncateHash(dstHash))
	}

	return nil
}

func (a *app_) computeFileHash(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var h hash.Hash
	if a.verify == verifySHA256 {
		h = sha256.New()
	} else {
		h = md5.New()
	}

	buf := make([]byte, copyBufSize)
	for {
		if a.ctrl.waitIfPaused() || a.ctrl.isCancelled() {
			return "", errCancelled
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := h.Write(buf[:n]); werr != nil {
				return "", werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func truncateHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// เก็บ import storage ไว้เผื่อขยายการรองรับ URI แบบอื่นในอนาคต
//var _ = storage.NewFileReader
