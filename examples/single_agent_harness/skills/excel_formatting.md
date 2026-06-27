# Excel Formatting — openpyxl Rules

## PHẢI đọc trước khi viết bất kỳ code openpyxl nào

---

## 0. FILE PATHS — quan trọng nhất

```
User upload file  →  đọc từ /uploaded/<filename>   (read-only trong Docker)
Output file       →  lưu vào /tmp/<filename>        (auto-export, auto-present)
```

**Workflow chuẩn cho uploaded file — rebuild toàn bộ:**
```python
wb = openpyxl.load_workbook('/uploaded/<tên_file_user_upload>')
ws = wb['Sheet1']

# ⚠️ BƯỚC BẮT BUỘC TRƯỚC KHI VIẾT — source file có thể có merged cells!
# Nếu không clear, sc() sẽ crash: AttributeError: 'MergedCell' object attribute 'value' is read-only
for merged in list(ws.merged_cells.ranges):
    ws.merged_cells.remove(merged)

# Xoá data cũ (optional nếu rebuild từ đầu)
ws.delete_rows(1, ws.max_row)

# Bây giờ mới viết lại bằng mc() và sc() bình thường
wb.save('/tmp/<tên_file_output>')   # auto-present
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

### ❌ Lỗi C: Border trên merged cell range — `isinstance(MergedCell)` check sai chỗ

Chỉ `.value` mới read-only trên MergedCell. `.border` **hoàn toàn settable** — không cần check.

Nếu `border_range()` skip MergedCell → rightmost cell của merged row (ví dụ I7 trong merge F7:I7) bị bỏ qua → **cạnh phải invoice trắng hoàn toàn** (3 cạnh có border, phải không).

```python
# SAI — skip MergedCell cho border → right edge thiếu border
def border_range(ws, r1, c1, r2, c2, bdr):
    for r in range(r1, r2 + 1):
        for c in range(c1, c2 + 1):
            cell = ws.cell(row=r, column=c)
            if not isinstance(cell, MergedCell):   # ← SAI: chỉ .value mới cần check này
                cell.border = bdr

# ĐÚNG — set border trực tiếp, không skip MergedCell
def border_range(ws, r1, c1, r2, c2, bdr):
    for r in range(r1, r2 + 1):
        for c in range(c1, c2 + 1):
            ws.cell(row=r, column=c).border = bdr   # MergedCell.border settable ✓
```

Tại sao đúng: khi apply `OUTER_BORDER` lên cả range A1:I35, cell I7 (MergedCell của merge F7:I7) nhận `right=MEDIUM_SIDE` → Excel render right border ở cạnh phải của column I. Internal borders của inner MergedCells (G7, H7) nằm trong vùng merge và bị Excel bỏ qua khi render.

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
    """Áp border cả range — bao gồm MergedCell (chỉ .value mới read-only, .border thì được set)."""
    for r in range(r1, r2 + 1):
        for c in range(c1, c2 + 1):
            ws.cell(row=r, column=c).border = bdr
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

### ❌ Lỗi D: Column reuse trap — cùng cột dùng cho 2 section có độ rộng khác nhau

Cột A vừa dùng cho STT (1, 2, 3 → cần width nhỏ) vừa chứa labels ở info section ("Ngân hàng / STK:" → cần width lớn). Set width theo STT → labels bị cắt. Set width theo label → STT column xấu và rộng thừa.

```python
# SAI — width=5 đủ cho STT nhưng label 17 ký tự → cắt nặng
ws.column_dimensions['A'].width = 5
sc(ws, info_row, 1, 'Ngân hàng / STK:', ...)   # chỉ thấy "Ngân"!

# SAI — width=19 đủ cho label nhưng STT column rộng xấu
ws.column_dimensions['A'].width = 19
sc(ws, table_row, 1, 1, ...)   # STT "1" bơi giữa ô rộng 19 chars
```

**ĐÚNG — Giải pháp: info section dùng merged cells, không phụ thuộc col width**

```python
# Bước 1: set col A width theo NHU CẦU TABLE (STT) — hẹp, gọn
ws.column_dimensions['A'].width = 5   # đủ cho "1","2",...

# Bước 2: info section labels → merge A:B (hoặc A:C) để có đủ chỗ hiển thị
# Merge span qua đủ cột để tổng width >= len(longest_label) + 2
# Ví dụ: col A (5) + col B (20) = 25 → đủ cho "Ngân hàng / STK:" (17 ký tự)
mc(ws, info_row, 1, info_row, 2, 'Ngân hàng / STK:', font=LABEL_FONT, align=A_LC)   # A:B merged
mc(ws, info_row, 3, info_row, 4, seller_bank_value, ...)                              # C:D value

# Bước 3: table section KHÔNG bị ảnh hưởng — merged cells ở info rows không đổi col width
sc(ws, table_row, 1, stt_number, ...)   # col A vẫn width=5, STT hiển thị đẹp
```

**Rule quan trọng**: Khi info section và table section **chia sẻ cùng cột** mà cần độ rộng khác nhau — **dùng `merge_cells` ở info section** để span qua đủ cột, thay vì tăng `column_dimensions` width (sẽ làm xấu table section).

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

**⚠️ `\n` vs `\` line continuation — rất dễ nhầm:**

```python
# SAI — đây là Python line continuation, KHÔNG phải newline trong string
header_text = 'Thành tiền\
trước thuế\
(Pre-tax)'
# → string thực tế: 'Thành tiền trước thuế (Pre-tax)'  (1 dòng, KHÔNG có \n)
# → header_text.count('\n') = 0 → n_lines = 1 → height quá thấp!

# ĐÚNG — dùng \n literal bên trong string để có newline thật
header_text = 'Thành tiền\ntrước thuế\n(Pre-tax)'
# → header_text.count('\n') = 2 → n_lines = 3 → height đúng
```

**Rule**: Để tạo header nhiều dòng trong Excel, **bắt buộc dùng `'\n'`** (escape sequence bên trong string). `\` ở cuối dòng source code chỉ là line continuation của Python — string kết quả không có newline.

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

### ❌ Lỗi G: `load_workbook` giữ lại merged cells của source → crash khi ghi

Source file có thể đã có merged cells. Sau `load_workbook()`, các merge đó vẫn tồn tại trong `ws`. Khi `sc()` gọi `ws.cell(row=r, column=c, value=v)` trên ô là MergedCell từ source → crash.

```python
# SAI — không clear merge từ source trước khi rebuild
wb = openpyxl.load_workbook('/uploaded/file.xlsx')
ws = wb['Sheet1']
sc(ws, 8, 3, 'value')  # Nếu (8,3) là MergedCell trong source → AttributeError!

# ĐÚNG — clear tất cả merged ranges của source TRƯỚC KHI viết bất kỳ ô nào
wb = openpyxl.load_workbook('/uploaded/file.xlsx')
ws = wb['Sheet1']
for merged in list(ws.merged_cells.ranges):   # list() vì sẽ modify trong loop
    ws.merged_cells.remove(merged)
ws.delete_rows(1, ws.max_row)                 # xoá data cũ nếu rebuild từ đầu
# Bây giờ mới viết lại bình thường
```

**Rule**: Mọi script rebuild từ source file → 2 dòng này **luôn phải có ngay sau khi mở workbook**, trước bất kỳ `sc()` hay `mc()` nào.

---

### ❌ Lỗi E: Row height tính từ một phía — bỏ quên phía còn lại

Info section có 2 nhóm cột (SELLER trái + BUYER phải) cùng chung một row. Nếu chỉ tính height theo seller value, sẽ bỏ quên buyer label (vd: "Bộ phận / Phòng ban:" 20 ký tự) cũng cần wrap → bị cắt ở đáy row.

```python
# SAI — chỉ tính theo content một phía
ws.row_dimensions[r].height = row_height_for_wrap(seller_value, sel_val_width)

# ĐÚNG — lấy max của TẤT CẢ ô có content trong row đó
for i, (s_label, s_val, b_label, b_val) in enumerate(info_rows):
    r = INFO_START + i
    ws.row_dimensions[r].height = max(
        row_height_for_wrap(s_label, sel_label_col_width),
        row_height_for_wrap(s_val,   sel_val_col_width),
        row_height_for_wrap(b_label, buy_label_col_width),
        row_height_for_wrap(b_val,   buy_val_col_width),
    )
```

---

### ❌ Lỗi F: Fill gap ở separator column trong colored header row

Khi tạo 2 header block (ví dụ SELLER `A5:D5` và BUYER `F5:I5`), cột E5 ở giữa nếu không được fill sẽ xuất hiện dải TRẮNG ngay giữa header màu — nhìn rất xấu.

```python
# SAI — E5 không fill → gap trắng chia đôi header xanh
mc(ws, 5, 1, 5, 4, '▶ NGƯỜI BÁN HÀNG (SELLER)', fill=DARK_BLUE, font=WHITE_BOLD, align=A_CC)
mc(ws, 5, 6, 5, 9, '▶ NGƯỜI MUA HÀNG (BUYER)',  fill=DARK_BLUE, font=WHITE_BOLD, align=A_CC)
# E5 = trắng → gap!

# ĐÚNG — fill separator column cùng màu
mc(ws, 5, 1, 5, 4, '▶ NGƯỜI BÁN HÀNG (SELLER)', fill=DARK_BLUE, font=WHITE_BOLD, align=A_CC)
ws.cell(row=5, column=5).fill = DARK_BLUE   # E5 separator — same fill, no content
mc(ws, 5, 6, 5, 9, '▶ NGƯỜI MUA HÀNG (BUYER)',  fill=DARK_BLUE, font=WHITE_BOLD, align=A_CC)
```

**Rule**: Mỗi khi có colored row mà layout dùng `merge_cells` cho từng block riêng lẻ, phải **explicitly fill TẤT CẢ ô không thuộc bất kỳ merge nào** trong row đó để tránh gap.

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
- [ ] Sau `load_workbook`: clear ALL merged ranges trước khi viết (`for m in list(ws.merged_cells.ranges): ws.merged_cells.remove(m)`)
- [ ] Output lưu vào `/tmp/<filename>.xlsx`
- [ ] Mọi `sc()` call dùng keyword args
- [ ] Mọi merge → chỉ ghi top-left cell
- [ ] `border_range()` KHÔNG có `if not isinstance(cell, MergedCell)` check — `.border` settable trên MergedCell (chỉ `.value` mới không)
- [ ] Cột số: width ≥ `len(str(max_value)) + số dấu phẩy + 3` — tránh `####`
- [ ] Cột text không wrap: width = `len(text dài nhất) + 2`
- [ ] Cột text CÓ wrap: set `row_dimensions[r].height` thủ công = `ceil(len / chars_per_line) × line_height + padding`
- [ ] Header có `\n`: height = `n_lines × 15 + 8`
- [ ] Cột số KHÔNG dùng `wrap_text=True`
- [ ] `number_format` là string, không phải object (ví dụ `'#,##0'` không phải `THIN_BORDER`)
- [ ] Nếu info section và table section chia sẻ cùng cột (vd: col A = STT + label): dùng `merge_cells` ở info section để span đủ cols, KHÔNG tăng col width theo label (sẽ làm xấu STT column)
- [ ] Header nhiều dòng dùng `'\n'` literal, KHÔNG dùng `\` line continuation của Python
- [ ] `row_dimensions[r].height` của info section rows lấy `max(...)` của TẤT CẢ cells trong row (seller label + value + buyer label + value)
- [ ] Colored header row có nhiều merged blocks: fill TẤT CẢ ô không thuộc merge nào (separator columns) cùng màu — tránh gap trắng
