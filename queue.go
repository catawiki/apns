package apns

import (
	"io"
	"log"
	"sync"
	"time"
)

// Используемые по умолчанию значения. При желании, вы можете их переопределить из своего приложения.
var (
	NotificationCacheSize = 1000            // размер кеша по умолчанию
	MaxFrameBuffer        = 65535           // максимальный размер пакета в байтах на отправку
	CacheLifeTime         = 5 * time.Minute // как долго хранятся отправленные сообщения
)

// notificationQueue описывает очередь сообщений на отправку. Уже отправленные уведомления так же хранятся
// в этой очереди и периодически очищаются от тех, чье время кеширования истекло.
type notificationQueue struct {
	list       []*notification // список элементов
	counter    uint32          // счетчик
	idUnsended int             // индекс первого еще не отосланного уведомления
	mu         sync.RWMutex    // блокировка асинхронного доступа
}

// newNotificationQueue возвращает новый инициализированную очередь на отправку и, одновременно, кеш уже
// отправленных уведомлений. С этим интервалом CacheLifeTime данный список проверяется и из него автоматически
// удаляются все отправленные сообщения, старше этого интервала.
func newNotificationQueue() *notificationQueue {
	var q = &notificationQueue{
		list: make([]*notification, 0, NotificationCacheSize),
	}
	go func() {
	loop:
		for { // бесконечный цикл проверки и очистки кеша
			time.Sleep(CacheLifeTime)                     // спим заданное количество времени
			var lifeTime = time.Now().Add(-CacheLifeTime) // время создания, после которого уведомления устарели
			trace("[clear]")                              // DEBUG
			q.mu.RLock()
			// перебираем все отправленные в обратном порядке, но только если первое не является отправленным
			for i := q.idUnsended; i > 0; i-- {
				// список всегда упорядочен по дате, поэтому достаточно найти первое вхождение
				// элемента, который уже "просрочен", а остальные - игнорировать
				if q.list[i-1].Sended.After(lifeTime) {
					continue // пропускаем не устаревшие
				}
				// мы нашли первое устаревшее уведомление, перебирая с конца
				// значит все остальные перед ним тоже устаревшие
				q.mu.RUnlock()
				q.mu.Lock()
				q.list = q.list[i:] // сохраняем очищенный список
				q.idUnsended -= i   // сдвигаем индекс последнего отосланного уведомления на кол-во удаленных
				q.mu.Unlock()
				continue loop // все обработано - уходим на глобальный повтор
			}
			q.mu.RUnlock()
			un("[clear]") // DEBUG
		}
	}()
	return q
}

// Add добавляет новые элементы в очередь на отправку. При добавлении автоматически назначается уникальный
// идентификатор, если он не был назначен до этого.
func (q *notificationQueue) Add(list ...*notification) {
	q.mu.Lock()
	for _, item := range list {
		if item.Id == 0 {
			q.counter++
			item.Id = q.counter
		}
	}
	q.list = append(q.list, list...)
	q.mu.Unlock()
}

// AddNotification генерирует и добавляет в очередь новое уведомление для каждого токена устройства,
// переданного в параметрах. В качестве шаблона используется сообщение в формате Notification.
// Если Notification содержит некорректные данные для уведомления, то возвращается ошибка и ни одного
// сообщения при этом в очередь добавлено не будет. Также проверяется длина токена устройства:
// если она не соответствует 32 байтам, то такие токены просто молча игнорируются.
func (q *notificationQueue) AddNotification(ntf *Notification, tokens ...[]byte) error {
	if len(tokens) == 0 {
		return nil
	}
	template, err := ntf.convert() // конвертируем сообщение во внутреннее представление
	if err != nil {
		return err
	}
	q.mu.Lock()
	for _, token := range tokens {
		if len(token) != 32 {
			continue // игнорируем токены устройств с неверным размером
		}
		var item = template.WithToken(token) // добавляем токен
		q.counter++
		item.Id = q.counter           // присваиваем уникальный идентификатор
		q.list = append(q.list, item) // помещаем в список на отправку
	}
	q.mu.Unlock()
	return nil
}

// WriteTo отправляет еще не отправленные сообщения в поток, и помечает их как отправленные в случае
// успешного завершения операции. В ответ возвращается общее количество байт, переданных в поток.
// Запись в поток ведется до тех пор, пока в списке есть хотя бы одно не отправленное уведомление
// или пока не случится ошибка.
//
// Для оптимизации запись в поток сообщений ведется сразу блоками, а не по одному. Это позволяет
// отправлять существенно больше сообщений за один раз, если они накопились в списке.
func (q *notificationQueue) WriteTo(w io.Writer) (total int64, err error) {
	var buf = getBuffer() // получаем из пулла байтовый буфер
	defer putBuffer(buf)  // освобождаем буфер после работы
	var sended int        // количество отправленных
	q.mu.RLock()
	// перебираем еще не отосланные сообщения
	for i, length := q.idUnsended, len(q.list); i < length; i++ {
		var ntf = q.list[i] // получаем уведомление на отправку из списка
		// если после добавления этого уведомления буфер не переполнится, то добавляем его на отправку
		if buf.Len()+ntf.Len() <= MaxFrameBuffer {
			if _, err = ntf.WriteTo(buf); err != nil { // сохраняем бинарное представление уведомления в буфере
				break // прерываем цикл при ошибке
			}
			ntf.Sended = time.Now() // помечаем время отправки
			if i < length-1 {
				continue // переходим к следующему элементу, если этот не последний
			}
		}
		// сюда мы попадаем, если буфер переполнен или мы добавили в него последний элемент списка
		var n int64             // количество отправленных данных
		n, err = buf.WriteTo(w) // отсылаем буфер сообщений
		total += n              // увеличиваем счетчик количества отправленных данных
		if err != nil {
			break // прерываемся, если ошибка
		}
		log.Printf("Sended %d messages (%d bytes)", i-sended, n)
		sended = i // сохраняем индекс последнего отправленного уведомления
	}
	if q.idUnsended < sended {
		q.mu.RUnlock()
		q.mu.Lock()
		q.idUnsended = sended + 1 // сдвигаем указатель еще не отправленных на следующее после последнего
		q.mu.Unlock()
		return
	}
	q.mu.RUnlock()
	return
}

// IsHasToSend возвращает true, если в списке есть неотправленные уведомления.
func (q *notificationQueue) IsHasToSend() bool {
	q.mu.RLock()
	var result = len(q.list) > q.idUnsended
	q.mu.RUnlock()
	return result
}

// ResendFromId находит в списке отправленных сообщение с таким идентификатором и переставляет указатель
// на отправку на него. Если в качестве второго параметра указано значение true, то найденное сообщение
// тоже игнорируется. Все сообщения до него удаляются из кеша.
func (q *notificationQueue) ResendFromId(id uint32, exclude bool) bool {
	q.mu.RLock()
	for i := 0; i < q.idUnsended; i++ {
		if q.list[i].Id != id { // находим сообщение с указанным идентификатором
			continue
		}
		q.mu.RUnlock()
		log.Printf("Message with error: %+v", q.list[i].PayloadMap())
		if exclude { // если указан флаг, что это уведомление нужно пропустить, то указываем на следующее
			i++
		}
		q.mu.Lock()
		q.list = q.list[i:] // удаляем все сообщения до найденного
		q.idUnsended = 0    // в списке остались только еще не отправленные
		q.mu.Unlock()
		return true
	}
	q.mu.RUnlock()
	return false
}
