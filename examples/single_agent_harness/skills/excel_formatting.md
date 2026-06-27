# Excel Formatting — openpyxl Rules

## PHẢI đọc trước khi viết bất kỳ code openpyxl nào

---

## 0. FILE PATHS — quan trọng nhất

```
User upload file  →  đọc từ /uploaded/<filename>   (read-only trong Docker)
Output file       →  lưu vào /tmp/<filename>        (auto-export, auto-present)
```

**Workflow chuẩn cho uploaded file:**
```python
# KHÔNG cần edit_xlsx cho user-uploaded file — nó đã có sẵn ở /uploaded/
wb = openpyxl.load_workbook('/uploaded/HoaDon_GTGT_7SP.xlsx')
# ... chỉnh sửa ...
wb_new.save('/tmp/HoaDon_Formatted.xlsx')   # auto-present, KHÔNG gọi present_artifact
```

**Workflow chuẩn cho session artifact (file agent đã tạo trước đó):**
```python
# Gọi edit_xlsx("filename.xlsx", "hướng dẫn") trước để stage vào /uploaded/
# Sau đó execute_python đọc từ /uploaded/ và lưu vào /tmp/
```

---

## 1. LỖI PHỔ BIẾN — tránh ngay từ đầu

### ❌ Lỗi A: `MergedCell attribute 'value' is read-only`
```python
# SAI — ghi vào ô con của merged range
ws.merge_cells('B2:E2')
ws.cell(row=2, column=3).value = 'text'   # crash!

# ĐÚNG — ghi vào top-left TRƯỚC KHI hoặc SAU KHI merge (chỉ ô top-left)
ws.cell(row=2, column=2, value='text')
ws.merge_cells(start_row=2, start_column=2, end_row=2, end_column=5)
```

### ❌ Lỗi B: `CellStyle.alignment should be Alignment but value is Border`
Nguyên nhân: nhầm thứ tự positional args.
```python
# SAI — THIN_BORDER rơi vào vị trí alignment
set_cell(ws, r, 7, '', FONT, FILL, THIN_BORDER)

# ĐÚNG — luôn dùng keyword args khi bỏ bớt tham số
set_cell(ws, r, 7, '', font=FONT, fill=FILL, border=THIN_BORDER)
```

### ❌ Lỗi C: Border trên merged cell range
```python
# Sau merge, chỉ set border cho top-left; dùng helper bỏ qua MergedCell:
from openpyxl.cell.cell import MergedCell
def border_range(ws, r1, c1, r2, c2, border):
    for r in range(r1, r2 + 1):
        for c in range(c1, c2 + 1):
            cell = ws.cell(row=r, column=c)
            if not isinstance(cell, MergedCell):
                cell.border = border
```

---

## 2. TEMPLATE HELPER — copy ngay vào code

```python
import openpyxl
from openpyxl.styles import Font, PatternFill, Alignment, Border, Side
from openpyxl.utils import get_column_letter
from openpyxl.cell.cell import MergedCell

# ── Helpers ─────────────────────────────────────────────────────────────
def sc(ws, r, c, v, *, font=None, fill=None, align=None, border=None, nf=None):
    """Set cell — LUÔN keyword args để tránh nhầm vị trí."""
    cell = ws.cell(row=r, column=c, value=v)
    if font:   cell.font          = font
    if fill:   cell.fill          = fill
    if align:  cell.alignment     = align
    if border: cell.border        = border
    if nf:     cell.number_format = nf
    return cell

def mc(ws, r1, c1, r2, c2, v, *, font=None, fill=None, align=None, border=None):
    """Merge rồi set top-left — không bao giờ ghi vào ô con."""
    ws.merge_cells(start_row=r1, start_column=c1, end_row=r2, end_column=c2)
    return sc(ws, r1, c1, v, font=font, fill=fill, align=align, border=border)

def border_range(ws, r1, c1, r2, c2, bdr):
    """Áp border cả range, bỏ qua MergedCell."""
    for r in range(r1, r2 + 1):
        for c in range(c1, c2 + 1):
            cell = ws.cell(row=r, column=c)
            if not isinstance(cell, MergedCell):
                cell.border = bdr
```

---

## 3. LAYOUT — tránh ô bị tràn chữ

```python
# ── Độ rộng cột (ví dụ invoice 9 cột) ──────────────────────────────────
col_widths = {1: 5, 2: 45, 3: 9, 4: 9, 5: 16, 6: 18, 7: 10, 8: 16, 9: 18}
for col, w in col_widths.items():
    ws.column_dimensions[get_column_letter(col)].width = w

# ── Chiều cao dòng (bắt buộc set nếu có wrap_text) ──────────────────────
ws.row_dimensions[1].height = 22   # dòng thường
ws.row_dimensions[2].height = 35   # dòng tiêu đề lớn
ws.row_dimensions[header_row].height = 42  # header bảng (multi-line)
# Dòng data: min 22; nếu tên hàng dài đặt 28–32
for r in data_rows:
    ws.row_dimensions[r].height = 28

# ── Alignment cho merged cell header ────────────────────────────────────
A_CC  = Alignment(horizontal='center', vertical='center', wrap_text=True)
A_LC  = Alignment(horizontal='left',   vertical='center', wrap_text=True)
A_RC  = Alignment(horizontal='right',  vertical='center')
```

---

## 4. DEBUG WORKFLOW — patch, không viết lại

```
Lỗi style/type → KHÔNG xóa và viết lại toàn bộ script!

1. Đọc traceback → lấy line number
2. grep_code('.last_run.py', 'set_cell|BORDER|alignment')
3. patch_code('.last_run.py', <dòng sai>, <dòng đúng>)
4. execute_file('.last_run.py')
```

Chỉ viết lại từ đầu khi: sai logic cấu trúc lớn (ví dụ đọc sai source file).

---

## 5. CHECKLIST TRƯỚC KHI SAVE

- [ ] Source file đọc từ `/uploaded/<filename>` (nếu user upload) hoặc `/uploaded/<filename>` (nếu edit_xlsx đã stage)
- [ ] Output lưu vào `/tmp/<filename>.xlsx`
- [ ] Mọi `sc()` call dùng keyword args
- [ ] Mọi merge → chỉ ghi top-left cell
- [ ] `border_range()` thay vì loop ghi thẳng vào merged cells
- [ ] `row_dimensions[r].height` set cho mọi dòng có nội dung
- [ ] `column_dimensions[letter].width` set đủ rộng (cột label ≥ 10, cột nội dung ≥ 30)
- [ ] `number_format` là string, không phải object (ví dụ `'#,##0'` không phải `THIN_BORDER`)
