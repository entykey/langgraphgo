# Excel Formatting — openpyxl Rules

## Bắt buộc đọc trước khi viết bất kỳ code openpyxl nào

---

## 1. QUY TRÌNH CHUẨN — đọc trước, viết sau

```
1. read_excel(filename)        → xem structure, merged cells, số sheet
2. load_skill("excel_formatting") → đọc rules này (đang làm)
3. execute_python(code)        → chạy lần đầu
4. Nếu lỗi → patch_code + execute_file  ← KHÔNG viết lại từ đầu
```

---

## 2. LỖI PHỔ BIẾN VÀ CÁCH TRÁNH

### ❌ Lỗi 1: `MergedCell attribute 'value' is read-only`
Nguyên nhân: ghi vào ô không phải top-left của merged range.

```python
# SAI — C2 là merged cell, không ghi được
ws.merge_cells('B2:E2')
ws.cell(row=2, column=3).value = 'text'   # AttributeError!

# ĐÚNG — ghi vào top-left (B2) trước hoặc sau khi merge
ws.cell(row=2, column=2, value='text')    # ghi trước
ws.merge_cells('B2:E2')                   # rồi merge
# HOẶC: ghi sau khi merge nhưng đúng top-left
ws.merge_cells('B2:E2')
ws.cell(row=2, column=2).value = 'text'  # B2 = top-left, OK
```

**Khi vẽ border trên vùng có merged cells:**
```python
# Sau khi merge, KHÔNG ghi style cho các ô bị merge — chỉ top-left
def apply_border_range(ws, r1, r2, c1, c2, border):
    from openpyxl.cell.cell import MergedCell
    for r in range(r1, r2 + 1):
        for c in range(c1, c2 + 1):
            cell = ws.cell(row=r, column=c)
            if not isinstance(cell, MergedCell):
                cell.border = border
```

---

### ❌ Lỗi 2: `CellStyle.alignment should be Alignment but value is Border`
Nguyên nhân: nhầm thứ tự positional args trong hàm helper `set_cell`.

```python
# Hàm helper thường viết:
def set_cell(ws, row, col, value, font=None, fill=None, alignment=None, border=None, nf=None):
    ...

# SAI — THIN_BORDER vào vị trí alignment, thiếu alignment arg
set_cell(ws, r, 7, '', TOTAL_FONT, TOTAL_BG, THIN_BORDER)
#                                              ↑ đây là alignment slot!

# ĐÚNG — luôn dùng keyword args khi bỏ bớt tham số
set_cell(ws, r, 7, '', font=TOTAL_FONT, fill=TOTAL_BG, border=THIN_BORDER)
```

**Quy tắc vàng: LUÔN dùng keyword args cho style functions.**

---

### ❌ Lỗi 3: `TypeError: expected str` khi save
Nguyên nhân: truyền object (Font, Border...) vào trường chỉ nhận str.

```python
# SAI
ws.cell(row=1, column=1).number_format = THIN_BORDER  # Border vào nf slot

# ĐÚNG
ws.cell(row=1, column=1).number_format = '#,##0'
```

---

### ❌ Lỗi 4: `PatternFill(fill_type=None)` vs `None`
```python
# Để ô không có màu nền:
fill = PatternFill(fill_type=None)   # ĐÚNG — explicit no-fill
fill = None                           # Cũng OK — không set fill
fill = PatternFill()                  # Có thể gây lỗi tùy version
```

---

## 3. TEMPLATE CHUẨN — dùng ngay, không tự nghĩ lại

```python
import openpyxl
from openpyxl.styles import Font, PatternFill, Alignment, Border, Side
from openpyxl.utils import get_column_letter
from openpyxl.cell.cell import MergedCell

wb = openpyxl.Workbook()
ws = wb.active

# ── Style objects (define once, reuse) ──────────────────────────────────
thin = Side(style='thin',   color='9E9E9E')
med  = Side(style='medium', color='1565C0')

THIN_BORDER   = Border(left=thin,  right=thin,  top=thin,  bottom=thin)
OUTER_BORDER  = Border(left=med,   right=med,   top=med,   bottom=med)

A_CC = Alignment(horizontal='center', vertical='center', wrap_text=True)
A_LC = Alignment(horizontal='left',   vertical='center', wrap_text=True)
A_RC = Alignment(horizontal='right',  vertical='center')

# ── Helper — LUÔN keyword args ───────────────────────────────────────────
def sc(r, c, v, *, font=None, fill=None, align=None, border=None, nf=None):
    cell = ws.cell(row=r, column=c, value=v)
    if font:   cell.font            = font
    if fill:   cell.fill            = fill
    if align:  cell.alignment       = align
    if border: cell.border          = border
    if nf:     cell.number_format   = nf
    return cell

# ── Merge + write (safe pattern) ─────────────────────────────────────────
def mc(r1, c1, r2, c2, v, *, font=None, fill=None, align=None, border=None):
    ws.merge_cells(start_row=r1, start_column=c1, end_row=r2, end_column=c2)
    return sc(r1, c1, v, font=font, fill=fill, align=align, border=border)

# ── Border trên range (bỏ qua MergedCell) ────────────────────────────────
def border_range(r1, c1, r2, c2, border):
    for r in range(r1, r2 + 1):
        for c in range(c1, c2 + 1):
            cell = ws.cell(row=r, column=c)
            if not isinstance(cell, MergedCell):
                cell.border = border

# ── Lưu output ───────────────────────────────────────────────────────────
wb.save('/tmp/output.xlsx')   # auto-exported và presented — KHÔNG gọi present_artifact
```

---

## 4. DEBUG WORKFLOW — KHÔNG viết lại từ đầu

Khi execute_python thất bại:
```
1. grep_code('.last_run.py', 'set_cell|BORDER|alignment')
   → tìm dòng lỗi theo traceback line number

2. patch_code('.last_run.py',
       old='set_cell(ws, r, 7, \"\", TOTAL_FONT, TOTAL_BG, THIN_BORDER)',
       new='sc(r, 7, \"\", font=TOTAL_FONT, fill=TOTAL_BG, border=THIN_BORDER)')

3. execute_file('.last_run.py')
```

Chỉ viết lại từ đầu nếu có lỗi logic cấu trúc lớn (ví dụ: sai cách build merged ranges). Lỗi style/type → luôn patch targeted.

---

## 5. CHECKLIST TRƯỚC KHI SAVE

- [ ] Mọi `set_cell` / `sc()` call dùng keyword args
- [ ] Không ghi vào `MergedCell` — dùng `border_range()` helper
- [ ] `PatternFill(fill_type=None)` cho ô không màu
- [ ] `number_format` là string, không phải object
- [ ] `wb.save('/tmp/<filename>.xlsx')` — lưu vào /tmp để auto-export
