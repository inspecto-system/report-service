package render

// Labels are the texts of a report in one language (ADR-019); API values are machine codes.
type Labels struct {
	Title, Property, Address, InspectedAt, Inspector, Status, CompletedAt, Notes string
	Summary, Severity, Count, Total, NoDefects                                   string
	Comparison, ComparedWith, Defect, Room, State                                string
	Checklist, Item, Result, Comment, Defects, Category, Element, Photos         string
	Generated, Page                                                              string
	Results, Severities, States, InspectionStatuses, ObservationStatuses         map[string]string
	ElementGroups                                                                map[string]string
}

var labels = map[string]Labels{
	"ru": {
		Title: "Акт осмотра", Property: "Объект", Address: "Адрес", InspectedAt: "Дата осмотра",
		Inspector: "Инспектор", Status: "Статус осмотра", CompletedAt: "Завершён", Notes: "Заметки",
		Summary: "Сводка дефектов", Severity: "Серьёзность", Count: "Количество", Total: "Всего",
		NoDefects:  "Дефектов не обнаружено",
		Comparison: "Сравнение с предыдущим осмотром", ComparedWith: "Предыдущий осмотр", Defect: "Дефект",
		Room: "Помещение", State: "Состояние",
		Checklist: "Чек-лист", Item: "Пункт", Result: "Результат", Comment: "Комментарий", Defects: "Дефекты",
		Category: "Категория", Element: "Элемент", Photos: "Фото",
		Generated: "Отчёт сформирован", Page: "Страница",
		Results: map[string]string{
			"OK": "В норме", "DEFECT": "Дефект", "NOT_APPLICABLE": "Неприменимо", "NOT_CHECKED": "Не проверено",
		},
		Severities: map[string]string{
			"CRITICAL": "Критический", "MAJOR": "Значительный", "MINOR": "Незначительный", "COSMETIC": "Косметический",
		},
		States: map[string]string{
			"NEW": "Новый", "RESOLVED": "Устранён", "REMAINING": "Остался", "NOT_CHECKED": "Не проверен",
		},
		InspectionStatuses: map[string]string{
			"DRAFT": "Черновик", "IN_PROGRESS": "Идёт осмотр", "COMPLETED": "Завершён",
		},
		ObservationStatuses: map[string]string{"OPEN": "Открыт", "RESOLVED": "Устранён"},
		ElementGroups: map[string]string{
			"WALLS": "Стены", "CEILING": "Потолок", "FLOOR": "Пол", "WINDOWS": "Окна", "DOORS": "Двери",
			"PLUMBING": "Сантехника", "ELECTRICAL": "Электрика", "OTHER": "Другое",
		},
	},
	"en": {
		Title: "Inspection report", Property: "Property", Address: "Address", InspectedAt: "Inspection date",
		Inspector: "Inspector", Status: "Inspection status", CompletedAt: "Completed", Notes: "Notes",
		Summary: "Defect summary", Severity: "Severity", Count: "Count", Total: "Total",
		NoDefects:  "No defects found",
		Comparison: "Comparison with the previous inspection", ComparedWith: "Previous inspection",
		Defect: "Defect", Room: "Room", State: "State",
		Checklist: "Checklist", Item: "Item", Result: "Result", Comment: "Comment", Defects: "Defects",
		Category: "Category", Element: "Element", Photos: "Photos",
		Generated: "Report generated", Page: "Page",
		Results: map[string]string{
			"OK": "OK", "DEFECT": "Defect", "NOT_APPLICABLE": "Not applicable", "NOT_CHECKED": "Not checked",
		},
		Severities: map[string]string{
			"CRITICAL": "Critical", "MAJOR": "Major", "MINOR": "Minor", "COSMETIC": "Cosmetic",
		},
		States: map[string]string{
			"NEW": "New", "RESOLVED": "Resolved", "REMAINING": "Remaining", "NOT_CHECKED": "Not checked",
		},
		InspectionStatuses: map[string]string{
			"DRAFT": "Draft", "IN_PROGRESS": "In progress", "COMPLETED": "Completed",
		},
		ObservationStatuses: map[string]string{"OPEN": "Open", "RESOLVED": "Resolved"},
		ElementGroups: map[string]string{
			"WALLS": "Walls", "CEILING": "Ceiling", "FLOOR": "Floor", "WINDOWS": "Windows", "DOORS": "Doors",
			"PLUMBING": "Plumbing", "ELECTRICAL": "Electrical", "OTHER": "Other",
		},
	},
}

// severityOrder lists the scale from the most severe, as the summary shows it (ADR-017).
var severityOrder = []string{"CRITICAL", "MAJOR", "MINOR", "COSMETIC"}

// LabelsFor returns the texts of the language; Russian for an unknown one.
func LabelsFor(locale string) Labels {
	if l, ok := labels[locale]; ok {
		return l
	}
	return labels["ru"]
}
