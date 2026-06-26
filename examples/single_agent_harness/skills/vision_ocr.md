# Skill: Vision / OCR

Quy trình phân tích ảnh chi tiết — đọc nội dung, trích xuất bảng, nhận diện vùng cụ thể.

## Khi nào cần load skill này

Load `vision_ocr` khi user yêu cầu **đọc/phân tích nội dung cụ thể** trong ảnh:
- Đọc văn bản, số liệu, bảng trong ảnh (OCR)
- Trích xuất dữ liệu có cấu trúc từ ảnh (hóa đơn, form, biểu đồ)
- Nhận diện vùng cụ thể theo yêu cầu ("đọc phần header", "lấy tổng tiền")

**KHÔNG cần load** nếu câu hỏi đơn giản như "ảnh này là gì" hay "mô tả ảnh" — gọi
`read_image` trực tiếp mà không cần skill chi tiết.

## Tool có sẵn (luôn active, không cần load thêm)

- `read_image(url_or_data)` — Phân tích ảnh bằng Gemini vision model.
  - Nhận: URL, data URI (`data:image/...;base64,...`), hoặc image_id server-side.
  - Khi user message chứa `[Ảnh đính kèm — gọi read_image("<id>")...]`, dùng đúng id đó.

## Quy trình phân tích chi tiết

1. **Gọi read_image** với image_id hoặc URL từ context.
2. **Đọc toàn bộ text** trong ảnh — đừng bỏ sót vùng nào.
3. **Xác định cấu trúc**:
   - Bảng → trích xuất thành dạng có header + rows rõ ràng.
   - Form/hóa đơn → map field name → value.
   - Biểu đồ → đọc tiêu đề, trục, giá trị chú thích.
4. **Trả lời đúng câu hỏi user** dựa trên dữ liệu thực đọc được, không suy diễn.

## Lưu ý quan trọng

- Nếu ảnh bị mờ/khó đọc, nói rõ phần nào không đọc được thay vì đoán mò.
- Số liệu tài chính (giá, tổng tiền): đọc chính xác, giữ nguyên đơn vị tiền tệ.
- Ngôn ngữ trong ảnh có thể khác tiếng Việt — transcribe đúng ngôn ngữ gốc,
  rồi dịch/giải thích nếu user cần.
