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
wb = openpyxl.load_workbook('/uploaded/<tên_file_user_upload>')
# ... chỉnh sửa ...
wb_new.save('/tmp/<tên_file_output>')   # auto-present, KHÔNG gọi present_artifact
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

**Nguyên tắc: không copy-paste magic numbers — reason về nội dung thực tế.**

```python
# ── Độ rộng cột ──────────────────────────────────────────────────────────
# Quyết định dựa trên nội dung sẽ điền vào từng cột:
#   - Cột số thứ tự / mã ngắn: 4–6
#   - Cột label / tên ngắn (10–15 ký tự): 12–18
#   - Cột mô tả dài (tên hàng, địa chỉ): ước tính độ dài text / 2, tối thiểu 30
#   - Cột số tiền (định dạng #,##0): 14–18 (đủ chứa 12 chữ số + dấu phân cách)
for col_idx, width in col_widths.items():   # col_widths do bạn tự định nghĩa theo nội dung
    ws.column_dimensions[get_column_letter(col_idx)].width = width

# ── Chiều cao dòng ────────────────────────────────────────────────────────
# BẮT BUỘC set thủ công khi dùng wrap_text=True, vì openpyxl không tự tính.
# Ước tính: (số ký tự nội dung / độ rộng cột) × 15 + 5, tối thiểu 20.
# Ví dụ:
#   - Dòng tiêu đề đơn: ~20–24
#   - Dòng tiêu đề lớn (font size 14–16): ~30–38
#   - Dòng header bảng có 2 dòng text: ~36–44
#   - Dòng data tên hàng dài (wrap): 24–32 tùy độ dài text và col width
ws.row_dimensions[r].height = <giá_trị_tính_theo_nội_dung>

# ── Alignment ────────────────────────────────────────────────────────────
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
- [ ] `row_dimensions[r].height` set thủ công cho mọi dòng dùng `wrap_text=True` (openpyxl không tự tính)
- [ ] `column_dimensions[letter].width` reason theo nội dung thực tế — không copy magic numbers từ file khác
- [ ] `number_format` là string, không phải object (ví dụ `'#,##0'` không phải `THIN_BORDER`)
