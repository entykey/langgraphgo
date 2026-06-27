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

## 3. LAYOUT — tránh `####` và chữ bị cắt

**openpyxl KHÔNG tự động điều chỉnh column width hay row height — phải set thủ công 100%.**

### 3a. Column width — tránh `####`

`####` xuất hiện khi cột quá hẹp để hiển thị số. Rule:

```python
import math

def col_width_for_number(max_value, number_format='#,##0'):
    """Tính width tối thiểu cho cột số dựa trên giá trị lớn nhất."""
    # Số chữ số + dấu phân cách ngàn + padding Excel (~2)
    formatted_len = len(f'{max_value:,.0f}') + 2
    return max(formatted_len, 10)   # tối thiểu 10

def col_width_for_text(texts, col_width_hint=None):
    """Tính width cho cột text dựa trên nội dung thực tế."""
    max_len = max((len(str(t)) for t in texts if t), default=10)
    if col_width_hint:
        # Nếu có wrap_text: width tùy ý, nhưng row height phải tăng tương ứng
        return col_width_hint
    return min(max_len + 2, 60)   # không quá 60, cộng 2 padding

# Ví dụ:
# Cột số tiền max 259,490,000 → len("259,490,000") = 11, +2 = 13 → width 13–16
# Cột tên hàng max 45 ký tự → width 30 + wrap_text → phải set row height
```

**Rule nhanh:**
- Cột số (`#,##0`): width = `len(str(max_value)) + len(dấu phẩy) + 3`, tối thiểu 12
- Cột text không wrap: width = `len(text dài nhất) + 2`
- Cột text có wrap: tự chọn width, nhưng **bắt buộc tính row height tương ứng**

---

### 3b. Row height — tránh chữ bị cắt khi wrap_text

**openpyxl mặc định row height = 15pt, KHÔNG tự expand khi wrap_text=True.**  
Nếu không set height thủ công → text bị cắt, chỉ thấy 1 dòng dù nội dung 3 dòng.

```python
import math

def row_height_for_wrap(text, col_width, font_size=10):
    """
    Tính row height cần thiết khi wrap_text=True.
    
    col_width: độ rộng cột tính bằng character width của Excel
    font_size: cỡ chữ (pt)
    
    Excel character width ≈ font_size × 0.6 px; 1 dòng text ≈ font_size × 1.2 pt height.
    Heuristic đủ dùng:
    """
    if not text:
        return font_size * 1.5
    chars_per_line = max(1, int(col_width * 1.2))   # ~1.2 chars/unit width ở font 10
    lines = math.ceil(len(str(text)) / chars_per_line)
    return max(lines * (font_size * 1.5) + 4, font_size * 1.5 + 4)

# Dùng trong vòng lặp data rows:
for i, row_data in enumerate(data):
    r = start_row + i
    longest_text = max(row_data, key=lambda x: len(str(x)) if x else 0)
    ws.row_dimensions[r].height = row_height_for_wrap(
        longest_text,
        col_width=col_width_of_text_column,   # width của cột chứa text dài nhất
        font_size=10
    )
```

**Trường hợp đặc biệt:**
```python
# Header bảng có \n (newline) trong text → đếm số dòng literal
header_text = 'Tên hàng hóa\n(Description)'
n_lines = header_text.count('\n') + 1   # = 2
ws.row_dimensions[header_row].height = n_lines * 15 + 8   # 15pt/dòng + padding

# Tiêu đề font lớn (size 14): height = font_size * 2 + 4 tối thiểu
ws.row_dimensions[title_row].height = 14 * 2 + 4   # = 32
```

---

### 3c. Alignment — wrap_text phải đi kèm height

```python
# wrap_text=True VÔ NGHĨA nếu không set row height → text vẫn bị cắt
A_CC  = Alignment(horizontal='center', vertical='center', wrap_text=True)
A_LC  = Alignment(horizontal='left',   vertical='center', wrap_text=True)
A_RC  = Alignment(horizontal='right',  vertical='center')   # số thường không wrap

# Với cột số: KHÔNG dùng wrap_text — số không wrap, chỉ cần đủ col width
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
- [ ] Cột số: width ≥ `len(str(max_value)) + số dấu phẩy + 3` — tránh `####`
- [ ] Cột text không wrap: width = `len(text dài nhất) + 2`
- [ ] Cột text CÓ wrap: set `row_dimensions[r].height` thủ công = `ceil(len / chars_per_line) × line_height + padding`
- [ ] Header có `\n`: height = `n_lines × 15 + 8`
- [ ] Cột số KHÔNG dùng `wrap_text=True`
- [ ] `number_format` là string, không phải object (ví dụ `'#,##0'` không phải `THIN_BORDER`)
