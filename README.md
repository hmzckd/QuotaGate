# QuotaGate

QuotaGate, çok müşterili bir Go uygulamasının **isteği çalıştırmadan önce** müşteri ve işlem için izin kararı almasını sağlayan dar kapsamlı bir servis çalışmasıdır. Hedef, Go ile API sınırları, eşzamanlı kota güncellemesi, tekrar deneme ve bağımlılık arızası davranışını gösteren yerel bir portföy demosudur. QG-01–04 kalıcı günlük kota ve middleware kullanan örnek API'yi, QG-05 ayrı Redis hız ön kontrolünü, QG-06 birleşik hız/günlük kararını, QG-07 sınırlı istemci tekrarını, QG-08 iki kopyalı gerçek bağımlılık kesintisi ve toparlanmayı içerir. **v0.1 ve v0.2 kabulü tamamlandı**. QG-09 metrik ve correlation ID içeren logları, QG-10 zaman sınırlarını, düzenli kapanışı ve karar kayıtlarının saklama/temizlik komutunu ekler. QG-11, ara katmanın eklediği gecikmeyi tekrarlanabilir bir yükle ölçer ve kararları PostgreSQL ile uzlaştırır. QG-12 aynı imajları yerel Kubernetes'te iki karar servisi kopyasıyla çalıştırır. QG-13, yük altında Redis ya da PostgreSQL kesintisini ve toparlanmayı tek komutla gösterir, her kararı uzlaştırır.

## İstek akışı

1. Müşteri, kendi QuotaGate anahtarıyla örnek API'de `POST /jobs` çağırır.
2. Örnek API'deki middleware her **gelen istek** için yeni bir karar kimliği üretir ve `operation=job.create` için `POST /v1/decisions` çağırır. Geçici bağlantı/yanıt hatasında aynı kimlikle, toplam iki saniye içinde en fazla üç deneme yapar.
3. QuotaGate anahtardan müşteriyi belirler. `serve-combined` modunda önce PostgreSQL'deki kayıtlı kararı arar; yeni kararda Redis hız kontrolünü, ardından PostgreSQL günlük kotasını değerlendirip nihai sonucu kaydeder.
4. İzin verilirse örnek API handler'ı çalışır. Ret gelirse `429`, karar alınamazsa `503` döner ve handler çalışmaz. `GET /v1/usage` yalnızca anahtarın sahibinin kalıcı günlük kullanımını gösterir.

Günlük kullanım **başarıyla tamamlanan iş** değil, QuotaGate'in kabul edip hak ayırdığı karardır. Handler hata verse veya istemciye yanıt ulaşmasa da ayrılan hak geri verilmez. İstemcinin örnek API isteğini yeniden göndermesi yeni bir karar ve yeni bir kullanım olabilir. Bu servis dış yan etki için exactly-once güvencesi vermez.

Servis trafik proxy'si değildir: genel routing, load balancing, TLS sertifika yönetimi, WAF, OAuth/SSO, eklenti, ödeme/faturalama, Kafka/NATS ve genel API Gateway özellikleri kapsam dışıdır. Yerel örnek API yalnızca kararın nasıl kullanıldığını gösterir. AWS, bu planın bitiş koşulu değildir.

Sürüm ve görevler: [PLAN.md](PLAN.md). Devam noktası: [STATUS.md](STATUS.md).

## QG-01 yerel çalıştırma

Gerekenler: Go 1.27+, Docker Desktop/Compose; duman testi için PowerShell 7. Mevcut kod sağlık, müşteri/politika, karar ve kullanım uçları ile örnek API'yi içerir.

```powershell
Copy-Item .env.compose.example .env.compose
# .env.compose içindeki QG_DB_PASSWORD ve QG_ADMIN_TOKEN değerlerini yalnızca bu yerel kurulum için değiştirin.
docker compose -p quotagate-qg01 --env-file .env.compose up --build -d --wait
Invoke-WebRequest http://127.0.0.1:18080/health/live
Invoke-WebRequest http://127.0.0.1:18080/health/ready
Invoke-WebRequest http://127.0.0.1:18081/health/live
docker compose -p quotagate-qg01 --env-file .env.compose run --rm migrate
docker compose -p quotagate-qg01 --env-file .env.compose down
```

`/health/live` HTTP sürecinin çalıştığını, `/health/ready` PostgreSQL'e 1 saniye içinde erişilebildiğini gösterir. Migration dosyaları `migrations/` altındadır; uygulanan dosyanın checksum'ı kaydedilir ve sonraki çalıştırmada değişmiş dosya reddedilir. `down` hacmi silmez; yerel test verisinin temizliği ayrı ve açık bir karardır. `.env.compose` Git dışında tutulur. Compose gerçek çalıştırmasının durumu [STATUS.md](STATUS.md) içinde kayıtlıdır.

## QG-02 yönetim ve müşteri uçları

Bu sürümde yönetici ve müşteri uçları `Authorization: Bearer <token>` kullanır. `ADMIN_TOKEN` yalnızca yönetim uçlarında geçerlidir. Yeni müşteri anahtarı `qg_` önekli 256 bit rastgele değerdir; yalnızca oluşturma veya yenileme yanıtında görünür, veritabanında SHA-256 hash'i bulunur. Anahtarlar ve yönetici sırrı uygulama loglarına yazılmaz. Bu yerel HTTP demosu için düz HTTP yalnızca `127.0.0.1` üzerinden yayımlanır.

| İşlem | Uç | Sonuç |
| --- | --- | --- |
| Müşteri oluştur | `POST /v1/admin/customers` `{ "name": "A" }` | `201`, müşteri kimliği ve tek seferlik anahtar |
| Politika yaz | `PUT /v1/admin/customers/{id}/policies/{operation}` `{ "daily_limit": 100, "rate_limit_per_minute": 10 }` | `200`, sürümlü politika |
| Anahtar yenile | `POST /v1/admin/customers/{id}/rotate-key` | `200`, yeni tek seferlik anahtar; eski anahtar `401` |
| Müşteriyi kapat | `POST /v1/admin/customers/{id}/disable` | `204`, mevcut anahtar `401` |
| Kendi kimliğini oku | `GET /v1/me` | `200`, anahtardan bulunan müşteri |
| Kendi politikasını oku | `GET /v1/policies/{operation}` | `200`, anahtardan bulunan müşterinin politikası |

Yönetim yolları dışındaki müşteri uçları `customer_id` almaz. Politika adı örneği `job.create` biçimindedir. QG-02 yalnızca limitleri kaydediyordu; QG-03 ile günlük karar ve kullanım eklendi.

## QG-03 günlük karar ve kullanım

Müşteri anahtarıyla `POST /v1/decisions` gövdesi `{ "decision_id": "rastgele-en-az-16-karakter", "operation": "job.create" }` gönderilir. Yanıt `200` ve `allowed`, `reason`, `utc_day`, `daily_used`, `daily_limit`, `policy_version` alanlarını taşır. `allowed=false, reason=daily_quota` karar servisi yanıtıdır; örnek API middleware'i bunu `429` olarak kullanır. Tanımsız işlem `403`, aynı karar kimliğinin farklı işlemle kullanılması `409`, geçersiz anahtar `401`, depolama kesintisi `503` verir.

`GET /v1/usage?operation=job.create`, anahtarın sahibinin **bugünkü UTC** kullanımını, limitini ve `resets_at_utc` değerini döndürür. `customer_id` parametresi kabul edilmez. Gün, PostgreSQL'in `clock_timestamp()` zamanından seçilir. Aynı müşteri ve karar kimliğiyle tekrar, ilk kaydedilen sonucu politika veya gün değişse de döndürür; yeni karar kimliği yeni bir hak talebidir.

Günlük hak, izin kararı transaction'da kaydedilince ayrılır. Sonraki handler başarısız olsa bile geri verilmez. Müşteri satırı kilidi tekrarları sıralar; koşullu PostgreSQL artışı ve karar kaydı aynı transaction'dadır. Temel `serve` modunda `/v1/decisions` günlük kota uygular; `serve-combined` modunda önce Redis hız kontrolü de uygulanır. Harici yan etkinin exactly-once çalışması vaat edilmez.

## QG-04 middleware ve örnek API

`client.New("http://127.0.0.1:18080")` ile oluşturulan Go istemcisi birden fazla müşterinin eşzamanlı isteklerinde paylaşılabilir. `decisions.Middleware("job.create", handler)` müşteri anahtarını gelen `Authorization: Bearer ...` header'ından alır; gelen gövdeyi tüketmeden QuotaGate kararı ister. İstemci aynı karar kimliği/gövde/anahtarla en fazla üç deneme yapar; toplam iki saniyelik bütçe ve gelen isteğin iptali tüm denemeleri ve beklemeleri kapsar. Yönlendirmeler izlenmez; eksik, bozuk veya çelişkili karar yanıtı handler'ı çalıştırmaz. Uygulama kapanırken `decisions.Close()` boş bağlantıları kapatır.

Compose örnek API'yi `127.0.0.1:18081` üzerinde açar. `POST /jobs` izin halinde `201` ve süreç içinde numaralanmış `demo-N` iş kimliği verir. Kota reddi `429`, karar alınamaması `503`, geçersiz anahtar `401`, tanımsız işlem politikası `403` olur. Her izin handler'ı bir kez çalıştırır; diğer sonuçlarda handler çalışmaz. Gelen `Idempotency-Key` veya gövdedeki karar kimliği tekrar kullanımı sağlamaz: ayrı API isteği yeni karar ve hak talebidir.

`GET /demo/stats` tüm müşteriler için toplam `handler_calls` sayısını gösteren yerel kanıttır; örnek API yeniden başladığında sıfırlanır. Kalıcı müşteri kullanımı QuotaGate'in `GET /v1/usage?operation=job.create` ucundan alınır. Örnek iş gerçek bir işe veya harici API'ye bağlı değildir. Örnek `/health/live` yalnızca örnek HTTP sürecini gösterir; QuotaGate'in bağımlılık durumu kendi `/health/ready` ucundadır.

İki müşteri, ret, kesinti ve toparlanma demosu:

```powershell
docker compose -p quotagate-qg01 --env-file .env.compose up --build -d --wait
pwsh -File ./scripts/smoke-qg04.ps1
docker compose -p quotagate-qg01 --env-file .env.compose down
```

Betik iki geçici müşteri için günlük limit `2/3` yazar, anahtarları bellekte tutar ve aynı inbound header ile ayrı karar kimliklerini doğrular. QuotaGate `api` servisini kısa süre durdurup yeniden başlatır; ret ve kesintide handler sayacının değişmediğini kontrol eder. Sonuçları PostgreSQL sayaç/karar kayıtlarıyla uzlaştırır, uygulama loglarında sır arar ve yalnızca oluşturduğu iki müşterinin verisini temizler. Çalışan izole Compose ortamında başka trafik olmadan çalıştırın. Betik `.env.compose` içindeki portları okur; örnek API portu `QG_EXAMPLE_PORT` ile değiştirilebilir.

`go test ./... -count=1` middleware HTTP testlerini çalıştırır. Gerçek PostgreSQL testleri için ayrıca izole, migration uygulanmış veritabanını `QG_TEST_DATABASE_URL` ile verin; değişken yokken bu testler atlanır. Kabul koşularının sonuçları [STATUS.md](STATUS.md) içindedir.

## QG-05 ortak Redis ön kontrolü

`serve-rate-demo` modu, müşteri anahtarıyla `POST /demo/rate-checks` `{ "decision_id": "rastgele-en-az-16-karakter", "operation": "job.create" }` ucunu açar. Müşteri anahtardan, hız limiti PostgreSQL politikasından alınır. `200` yanıtı `allowed`, `reason=rate_allowed|rate_limited`, `rate_used`, `rate_limit`, `window_start_utc`, `resets_at_utc` alanlarını taşır. Bu bir **hız ön sonucu**dur; handler izni vermez, PostgreSQL günlük sayacını veya nihai karar tablosunu değiştirmez. Bu tarihsel demo modunda `/v1/decisions` günlük kota akışını kullanır; birleşik akış aşağıdaki `serve-combined` modundadır.

Lua script'i müşteri + işlem sayacını, Redis `TIME` değerinin düştüğü 60 saniyelik sabit pencereye göre artırır. Ret sayaç artırmaz. Sayaç anahtarının mutlak silinme zamanı pencere sonudur; müşteri + karar kimliğinin ön sonucu 24 saat tutulur. Aynı kimlik/işlem ilk izin veya ret sonucunu, eski limitini ve penceresini döndürür; tekrar saklama süresini uzatmaz. Başka işlemle aynı kimlik `409` olur. Anahtarlar müşteri/işlem/karar kimliklerinin hash'lerini kullanır; API anahtarı Redis'e verilmez. Script cache silinince `EVALSHA` → `EVAL` dönüşü vardır. [Redis Lua yürütmesi](https://redis.io/docs/latest/develop/programmability/eval-intro/), [TIME](https://redis.io/docs/latest/commands/time/) ve [PEXPIREAT](https://redis.io/docs/latest/commands/pexpireat/).

İki kopyalı yerel kabul ortamı:

```powershell
docker compose -p quotagate-qg05 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml up --build -d --wait
pwsh -File ./scripts/smoke-qg05.ps1
docker compose -p quotagate-qg05 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml down
```

QG-05 ayrı `quotagate-qg05_pgdata` hacmini kullanır. Varsayılan portlar birinci API `18080`, ikinci API `18082`, Redis `56379`; Redis yalnızca loopback üzerinden yayımlanır ve `QG_REDIS_PORT` ile değiştirilebilir. QG-01–04 ortamıyla aynı HTTP/DB portlarını kullandığından iki ortamı sırayla çalıştırın. Redis bu demoda parolasız, diske yazmadan ve `128mb/noeviction` sınırıyla çalışır. Redis'in durması veya veri kaybı ön sonuçları kaybettirir; kalıcı günlük kullanım PostgreSQL'de kalır. Sabit pencere sınırında iki pencerenin kapasitesi kısa bir aralıkta kullanılabilir.

Betik iki kopyaya 60 benzersiz istek gönderir ve limit 7 altında 7 ön izin/53 ret bekler; 20 eşzamanlı aynı kimlikli isteğin tek ön hak tüketmesini, müşteri/işlem ayrımını, politika değişimi sonrası ilk sonucun korunmasını ve TTL'leri doğrular. PostgreSQL günlük kullanım/karar kayıtlarının oluşmadığını ve loglarda sır bulunmadığını kontrol eder; yalnızca kendi test müşterilerini ve Redis anahtarlarını siler.

Gerçek Redis testleri için `QG_TEST_REDIS_URL=redis://127.0.0.1:56379/15` verin ve `go test ./... -count=1` çalıştırın. Testler rastgele müşteri alanlarını temizler ve script cache dönüşünü doğrulamak için **izole test Redis'inde** `SCRIPT FLUSH` kullanır. Pencere sınırı testi, üretim Lua script'inin yalnızca saat ifadesini kontrollü değerle değiştirir; ayrı test gerçek Redis `TIME` ve mutlak key expiry'yi doğrular.

## QG-06 birleşik ve kalıcı karar

`serve-combined`, aynı `POST /v1/decisions` sözleşmesinde PostgreSQL kayıtlı sonuç → Redis ön kontrol → PostgreSQL günlük kota/nihai sonuç sırasını uygular. Yanıt `200` ve `{allowed, reason, utc_day, daily_used, daily_limit, policy_version}` biçimindedir. `reason`, `allowed`, `daily_quota` ve `rate_limited` değerlerini ayırır. Redis retleri de kalıcı karar olur; günlük sayaç oluşturmaz veya artırmaz. Müşteri kimliği anahtardan alınır; yabancı `customer_id` gövdesi ve query parametreleri `400` olur.

PostgreSQL transaction'ı kayıtlı karar aranmadan önce açılır. Müşteri satırı ve güncel politika kilitleri, bağlamla sınırlı Redis çağrısı ve kalıcılaştırma boyunca tutulur. Aynı müşteri kararları sıralanır; kayıtlı sonuç bulunduğunda Redis çağrılmaz. Gün, Redis kontrolünden sonra PostgreSQL `clock_timestamp()` değerinden seçilir; günlük sayaç ve nihai kayıt birlikte commit edilir. Bu yaklaşım müşteri başına eşzamanlılığı sınırlar ve Redis gecikmesi boyunca PostgreSQL bağlantısı tutar; performans ölçümü QG-11'dedir.

| Hata veya sonuç | Redis kapasitesi | PostgreSQL günlük kullanım ve karar |
| --- | --- | --- |
| Kimlik/politika/ilk DB okuması başarısız | Redis çağrılmaz | Yeni hak veya nihai kayıt yok |
| Redis çağrısı başarısız veya yanıtı kayıp | Lua tamamlandıysa kapasite tüketilmiş olabilir | Transaction geri alınır; `503` |
| Redis hız reddi | Yeni kapasite tüketmez | Günlük artmaz; `rate_limited` kaydedilir |
| Redis izin verdi, günlük kota dolu | Tüketilen kapasite korunur | Günlük artmaz; `daily_quota` kaydedilir |
| Redis izin verdi, PostgreSQL yazımı başarısız | Tüketilen kapasite korunur | Günlük artış ve kayıt geri alınır; `503` |
| PostgreSQL commit yanıtı belirsiz veya HTTP yanıtı kayıp | Tüketilen kapasite korunur | Commit olmuş olabilir; aynı kimlikle tekrar kayıtlı sonucu bulur |
| Nihai kararın aynı müşteri/işlem/kimlikle tekrarı | Redis'e başvurulmaz | İlk gün, kullanım, limit ve sürüm döner |

İki depo arasında ortak transaction veya hız hakkı iadesi yoktur. PostgreSQL'e ulaşmamış bir ön sonucun tekrarı Redis'in 24 saatlik kaydını kullanır. Arada politika güncellendiyse bu ön sonuç eski hız limitini korur; nihai PostgreSQL kararı günlük limit/sürümü o transaction'daki güncel politikadan alır. Kalıcılaştırılmış kararlar politika değişiminden etkilenmez. Redis veri kaybında tamamlanmamış ön sonuç devamlılığı garanti edilmez.

İki kopya ve middleware için yerel kabul:

```powershell
docker compose -p quotagate-qg06 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml up --build -d --wait
pwsh -NoProfile -File ./scripts/smoke-qg06.ps1
docker compose -p quotagate-qg06 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml down
```

QG-06 kendi `quotagate-qg06_pgdata` hacmini kullanır. Önceki demolarla aynı portları kullanır; sırayla çalıştırın. Birleşik mod yalnızca nihai karar ucunu açar; QG-05 ön kontrol ucu bu modda yoktur. Readiness PostgreSQL ve Redis'i kontrol eder. Redis erişilemese de mevcut PostgreSQL kararının tekrarı dönebilir; yeni kararda bağımlılık hatası `503` olur.

Betik dört geçici müşteri oluşturur. İki kopyaya 60 benzersiz çağrı günlük limit `3`, hız limiti `7` ile `3 allowed / 4 daily_quota / 53 rate_limited` verir. Yirmi aynı kimlikli çağrı bir günlük/hız hakkı kullanır. Bir müşteri için geçici PostgreSQL constraint'i, Redis kontrolünden sonraki karar yazımını bozarak `503` ve korunmuş Redis kapasitesini gösterir; aynı kimlikle tekrar sonucu tamamlar. Örnek `/jobs` için hız ve günlük retleri `429` olur, handler yalnızca iki izin için çalışır. PostgreSQL kullanım/karar/izin/günlük ret/hız ret toplamı `6|67|6|6|55` ile uzlaştırılır; loglarda sır aranır. `finally` geçici constraint'i, yalnızca kendi müşterilerini ve Redis anahtarlarını temizler. Başka trafik olmayan izole ortamda çalıştırın.

Birleşik entegrasyon testleri için migration uygulanmış izole PostgreSQL'i `QG_TEST_DATABASE_URL`, test Redis'ini `QG_TEST_REDIS_URL` ile verin. Her iki değişken varsa `go test ./... -count=1 -timeout=90s`, gerçek iki bağımlılıkta yarışları, Redis yanıt kaybını, PostgreSQL INSERT hatasında günlük artışın geri alınmasını, kalıcı tekrarın Redis'i atlamasını ve HTTP yanıt kaybını doğrular. Değişkenler eksikse ilgili entegrasyon testleri atlanır. Testler yalnızca rastgele oluşturdukları müşteri verisini temizler; hata enjeksiyonu kısa süreli, müşteriye özgü bir constraint eklediğinden paylaşılan/üretim veritabanında çalıştırmayın.

## QG-07 sınırlı tekrar ve handler sınırı

`Client.Decide`, tek karar çağrısında aynı `decision_id`, işlem, müşteri anahtarı ve gövdeyi korur. En fazla **üç deneme**, deneme başına **600 ms**, aralarda **50 ms / 100 ms** bekleme ve hepsini kapsayan **iki saniyelik toplam bütçe** vardır. Gelen bağlam daha erken sona ererse bütçeyi kısaltır. Bekleme ve yanıt gövdesi okuma da bütçeye dahildir; deneme sayısı veya bütçe dolunca `503` ve çalışmayan handler sonucu alınır.

| Sonuç | Tekrar |
| --- | --- |
| Bağlantı kopması, EOF/yarım HTTP gövdesi, deneme timeout'u | Bütçe ve deneme sayısı kaldıysa aynı kimlikle |
| HTTP `502`, `503`, `504` | Bütçe ve deneme sayısı kaldıysa aynı kimlikle |
| Nihai `allowed`, `daily_quota`, `rate_limited` | Yok; handler veya `429` |
| HTTP `401`, `403`, `409`, diğer HTTP durumları ve yönlendirme | Yok |
| TLS doğrulama hatası, bozuk/çelişkili/eksik/4096 bayttan büyük karar | Yok; handler çalışmaz |
| Gelen bağlamın iptali veya toplam bütçenin bitmesi | Yok; handler çalışmaz |

İlk deneme PostgreSQL'e commit edip yanıtını kaybettiyse tekrar kalıcı kararı bulur; günlük ve Redis hakkı tekrar harcanmaz. Bütün yanıtlar kaybolursa handler çalışmadan `503` dönebilir, fakat kalıcı hak ayrılmış olabilir. Aynı karar kimliğiyle manuel tekrar ilk sonucu gösterir. Ayrı bir `POST /jobs`, aynı gövde ve `Idempotency-Key` header'ına sahip olsa da yeni karar kimliği üretir; QuotaGate handler yan etkilerini tekilleştirmez. Bu sürümde her deneme aynı servis adresine gider; kopyalar arası failover istemcide uygulanmıyor.

Yerel Go araç zinciriyle iki Compose kopyasına karşı kabul:

```powershell
docker compose -p quotagate-qg07 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml up --build -d --wait
# Host Go modül önbelleği ilk kez hazırlanacaksa: go mod download
pwsh -NoProfile -File ./scripts/smoke-qg07.ps1
docker compose -p quotagate-qg07 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml down
```

QG-07 ayrı `quotagate-qg07_pgdata` hacmi kullanır; önceki demolarla aynı portları kullandığından sırayla çalıştırın. Betik iki gerçek Compose API kopyasını kullanır; **hostta çalışan test proxy'si ve örnek API handler'ı**, commit edilmiş yanıtı düşürür veya bekletir. Üretim servisine hata enjeksiyonu ucu eklenmez. Proxy bu testte denemeleri iki kopyaya sırayla iletir; istemci tek proxy adresini görür. Bu test, servis adresi failover'ı veya gerçek bağımlılık kesintisinden toparlanma kanıtı değildir; kesinti demosu QG-08'dedir.

Kabul dört geçici müşteride ilk yanıt kaybından toparlanmayı, bütün yanıtların kaybında üç denemede kapanmayı, manuel kalıcı tekrarı, farklı inbound kimliklerini, kısa caller deadline'ını ve toplam iki saniyelik bütçeyi doğrular. `/demo/stats` handler sayacı ve her müşteri için PostgreSQL kullanım/karar/izin sayıları ile Redis kapasitesi uzlaştırılır. Test, yalnızca kendi müşteri/Redis verisini ve yerel test sunucularını temizler; betik uygulama loglarında sır arar ve test env değerlerini eski haline getirir.

`go test ./client -count=1` bağlantı/gövde kaybı, `502/503/504`, timeout/iptal, terminal cevaplar ve 30 eşzamanlı inbound istekte 60 deneme/30 ayrı karar kimliğini doğrular. `QG_TEST_DATABASE_URL` ve `QG_TEST_REDIS_URL` varsa `TestExampleRetryAfterCommitWithPostgres`, gerçek iki bağımlılığa bağlı handler ile commit sonrası kaybı test eder. `TestQG07ComposeRetry` ise ayrıca iki `QG_TEST_API_URL`/`QG_TEST_API_URL_2` ve `QG_TEST_ADMIN_TOKEN` ister; betik bunları bellekte ayarlar. Canlı Compose testi Redis DB `0`, genel entegrasyon/race koşusu izole test DB `15` kullanır.

## QG-08 iki kopyalı kesinti ve toparlanma

`compose.qg08.yaml`, iki `serve-combined` kopyasını aynı PostgreSQL ve Redis'e bağlar; örnek API birinci kopyayı kullanır. Temel Compose dosyası üzerine tek ek yeterlidir:

```powershell
docker compose -p quotagate-qg08 --env-file .env.compose -f compose.yaml -f compose.qg08.yaml up --build -d --wait
pwsh -NoProfile -File ./scripts/smoke-qg08.ps1
docker compose -p quotagate-qg08 --env-file .env.compose -f compose.yaml -f compose.qg08.yaml down
```

QG-08 ayrı `quotagate-qg08_pgdata` hacmini kullanır. Varsayılan portlar QuotaGate `18080/18082`, örnek API `18081`, PostgreSQL `55432`, Redis `56379`; hepsi loopback üzerinden açılır. Önceki demolarla aynı portları kullandığından ortamları sırayla çalıştırın. **Boş, izole test PostgreSQL/Redis'i ve başka trafik olmayan ortamı kullanın.** Betik başlangıçta müşteri/sayaç/karar tablolarının ve Redis demo DB'sinin boş olduğunu kontrol eder. Redis durdurulup başlatılacağı için bu ortamda başka veri tutulmamalıdır.

Üç geçici müşteriyle ortak limitler ve toparlanma sınanır:

| Müşteri | Günlük / dakika limiti | İki kopyaya eşzamanlı çağrı | Beklenen nihai sonuç |
| --- | --- | --- | --- |
| Ortak hız | `1000 / 7` | `60` benzersiz karar | `7 allowed / 53 rate_limited` |
| Ortak günlük kota | `3 / 1000` | `40` benzersiz karar | `3 allowed / 37 daily_quota` |
| Toparlanma | `4 / 1000` | Örnek API ve doğrudan kararlar | Kesintilerde handler çalışmaz; toparlanınca günlük kullanım `4` olur |

Yük iki QuotaGate kopyasına dönüşümlü dağıtılır. Hız testi gerçek Redis dakika penceresinde çalışır; günlük kota PostgreSQL'de paylaşılır. Sonra betik yalnızca bağımlılık konteynerlerini durdurup başlatır:

| Kesinti | İki kopyada live / ready | Yeni karar ve örnek `/jobs` | Kayıtlı kararın tekrarı | Handler ve kalıcı sayaçlar |
| --- | --- | --- | --- | --- |
| Redis kapalı | `200 / 503` | `503` | PostgreSQL'den `200`, ilk sonuç korunur | Artmaz |
| PostgreSQL kapalı | `200 / 503` | `503` | `503` | Artmaz; DB geri gelince önceki kayıtlar korunur |

Redis bu demoda disk kalıcılığı olmadan çalışır. Yeniden başlatma hız sayaçlarını ve ön sonuçları kaybettirir. PostgreSQL'deki nihai kararın tekrarı Redis'i doldurmaz; yeni karar yeniden hız kapasitesi kullanır. Bu kabul hız verisinin kesinti boyunca korunmasını vaat etmez. PostgreSQL **çağrılardan önce tamamen durdurulduğunda** kimlik doğrulama başarısız olur ve yeni Redis ön sonucu oluşmaz. Redis kontrolünden sonra yaşanan DB hatasında kapasite tüketilmiş olabilir; bu ayrı hata noktası QG-06 kabulünde doğrulandı.

İki bağımlılık geri geldiğinde readiness ve yeni kararlar toparlanır. Betik iki QuotaGate kopyasının ve örnek API'nin konteyner kimliği, başlangıç zamanı ve restart sayısını karşılaştırarak uygulamalar yeniden başlatılmadan bağlantıların toparlandığını doğrular. Son günlük kullanım müşteri bazında `7/3/4`; PostgreSQL kullanım/karar/izin/günlük ret/hız ret/farklı kimlik toplamı `14|105|14|38|53|105` olur. Örnek API handler'ı üç izin için `+3` çalışır; son günlük kota reddi `429` verir. Doğrudan karar izinleri handler çağrısı değildir.

Betik loglarda müşteri anahtarlarını, yönetici sırrını ve DB parolasını arar. `finally` durdurduğu bağımlılıkları geri getirir, yalnızca oluşturduğu müşteri verisini ve Redis anahtarlarını temizler. `down` PostgreSQL hacmini korur. Bu yerel demo üretim HA veya otomatik failover kanıtı değildir. Birim, gerçek bağımlılık, race ve canlı Compose kabul sonuçları [STATUS.md](STATUS.md) içindedir.

## QG-09 karar metrikleri ve loglar

QuotaGate modları `GET /metrics` üzerinde [resmi Prometheus Go istemcisi](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus) ile süreç başına ayrı registry sunar. Exporter bağımlılıklara sorgu yapmaz; Redis/PostgreSQL kapalıyken de okunur. Bu yerel Compose kurulumunda aynı loopback HTTP portundadır. Prometheus sunucusu veya dashboard bu görevin parçası değildir.

| Metrik | Etiketler | Anlam |
| --- | --- | --- |
| `qg_decision_responses_total` | `outcome` | `allowed`, `rate_limited`, `daily_quota`, `unavailable` ve diğer sabit HTTP hata sınıfları |
| `qg_http_requests_total` | `route`, `method`, `status` | Tamamlanan HTTP handler'ının seçtiği yanıt durumu |
| `qg_http_request_duration_seconds` | `route`, `method` | Bağımlılık beklemeleri dahil handler süresi histogramı |
| `qg_dependency_errors_total` | `dependency`, `operation` | PostgreSQL/Redis işlem hataları; geçersiz anahtar, eksik politika ve kimlik çatışması hata sayılmaz |
| `qg_dependency_ready` | `dependency` | İlgili bağımlılığın son readiness probu: `1` hazır, `0` başarısız, `-1` henüz ölçülmedi |
| `qg_ready` | Yok | Son birleşik readiness probunun sonucu; aynı `1/0/-1` anlamı |

Readiness PostgreSQL ve varsa Redis'i **aynı bir saniyelik bağlam içinde eşzamanlı** kontrol eder; iki bağımlılığın sonucu ayrı görünür. Gauge değerleri son probe aittir; `/metrics` güncel bağlantı testi yapmaz. Liveness bağımlılıklardan ayrıdır. Prometheus'un readiness durumunu takip etmesi için `/health/ready` düzenli çağrılmalıdır; Compose healthcheck bunu yapar.

Bağımlılık hata etiketi başarısız çağrının yerini gösterir; timeout ve bağlam iptali de dahildir. Tek başına altyapı arızasının kök nedenini belirlemez.

Karar sayacı **HTTP yanıt girişimlerini** sayar: aynı karar kimliğinin tekrarı ve middleware'in her taşıma denemesi ayrı örnektir. `allowed` sayısı kalıcı ayrılan hak veya çalışan handler sayısı değildir. Yanıtın seçilmesi teslim edildiğini kanıtlamaz. Kalıcı kullanım PostgreSQL'den, örnek handler sayısı `/demo/stats` üzerinden uzlaştırılır. Süreç yeniden başladığında metrikler sıfırlanır; kopyalar kendi sayaçlarını sunar. `/metrics` okumaları istek metriğine ve HTTP loguna katılmaz.

Histogram sınırları saniye olarak `0.001/0.005/0.01/0.025/0.05/0.1/0.25/0.5/1/2/5/+Inf`. İki kopya Prometheus tarafından toplandığında karar ucunun yaklaşık p95 handler süresi:

```promql
histogram_quantile(0.95,
  sum by (le) (
    rate(qg_http_request_duration_seconds_bucket{route="/v1/decisions",method="POST"}[5m])
  )
)
```

Bu hesap bucket çözünürlüğüne bağlıdır; ağ gidiş/dönüş süresini veya middleware ek gecikmesini ölçmez. [Histogram ve quantile hesabı](https://prometheus.io/docs/practices/histograms/). Kontrollü p95 ek gecikme deneyi QG-11'dedir.

CLI JSON log kullanır. Her HTTP çağrısı servis tarafından üretilen yeni 128 bit `correlation_id` ve yanıtta aynı `X-Request-ID` alır; caller'ın verdiği header loga taşınmaz. `http_request` kaydı sabit route şablonu, sınırlı method, status, süre, bağımlılık hata bayrakları ve karar ucunda outcome içerir. Anahtar, DB/Redis URL'si, header, gövde, query, ham path, müşteri/karar kimliği veya ham bağımlılık hatası kaydedilmez. Başlangıç hatalarında yalnızca hata tipi loglanır; süreç hata koduyla kapanır. Bu correlation ID backend HTTP çağrısına aittir; farklı tekrarlar yeni ID alır, uçtan uca tracing değildir.

Etiketler sabit route şablonlarından ve sınırlı sonuç sınıflarından seçilir. Tanınmayan yollar `unmatched`, özel HTTP method'ları `OTHER` olur. Operation adı, correlation ID ve müşteri/karar kimlikleri etiket değildir. [Prometheus etiket kardinalitesi](https://prometheus.io/docs/practices/instrumentation/#do-not-overuse-labels).

Gerçek kesinti/metrik kabulü için ayrı proje ve hacim; QG-08 topolojisini kullanır:

```powershell
docker compose -p quotagate-qg09 --env-file .env.compose -f compose.yaml -f compose.qg08.yaml up --build -d --wait
pwsh -NoProfile -File ./scripts/smoke-qg09.ps1
docker compose -p quotagate-qg09 --env-file .env.compose -f compose.yaml -f compose.qg08.yaml down
```

Önceki demolarla aynı portlar kullanılır. Boş PostgreSQL/Redis, yeni API konteynerleri ve başka karar trafiği olmayan ortam gerekir. Betik üç geçici müşteri oluşturur; izin/hız/günlük ret/kalıcı tekrar, gerçek Redis ve PostgreSQL kesintileri, bağımlılık gauge'ları ve hata sınıfları, histogram örnekleri ve yanıt–JSON log correlation eşleşmesini kontrol eder. Caller header'ında yönetici sırrı ve tanınmayan URL'de müşteri/anahtar verisi göndererek telemetry'de bulunmadığını da doğrular. Yalnızca kendi müşteri/Redis verisini temizler ve durdurduğu bağımlılıkları geri getirir. Kabul sonuçları ve sonraki görev [STATUS.md](STATUS.md) içindedir.

## QG-10 zaman sınırları, kapanış ve karar saklama

### Zaman sınırları

Her katmanın sınırı bir öncekinden biraz uzundur. Böylece içteki sınır önce dolar ve istemci yine anlamlı bir yanıt alır.

| Katman | Sınır | Nerede |
| --- | --- | --- |
| Tek bağımlılık çağrısı | `3s` | `requestContext` (handler'lar) |
| Bütün istek | `4s` | `withRequestBudget` (`internal/httpapi/budget.go`) |
| Yanıt yazma | `5s` | `http.Server.WriteTimeout`; `ReadHeaderTimeout` ve `ReadTimeout` da `5s`, `IdleTimeout` `30s` |
| Düzenli kapanış | `10s` | `shutdownTimeout` (`cmd/quotagate/main.go`) |

İkinci katman, iki çağrının arka arkaya `3s` sürüp toplamda `6s` tutmasını engeller. `4s` bütçe `5s` yazma sınırının altında kalır, böylece geç kalan `503` yine istemciye ulaşır.

Bağlam sınırı olmayan bir çağrı için de alt sınırlar vardır:

- **PostgreSQL havuzu (`store.NewPool`):** en çok `10` bağlantı, bağlantı kurma `2s`. Sunucu tarafında `statement_timeout=5s` ve `idle_in_transaction_session_timeout=10s` oturum ayarı olarak gönderilir. Context biten sorguda pgx bağlantıyı kapatır; PostgreSQL bekleyen kilit isteğini bırakır ve havuz bağlantıyı geri alır. Sunucu ayarları, bağlam vermeyen ya da bağlantıyı kapatmadan kaybolan bir istemci için yedektir.
- **Redis (`ratelimit.New`, QG-05'ten beri):** bağlanma, okuma, yazma ve havuz bekleme `1s`; otomatik tekrar yok; context süresine uyulur.

Askıdaki bağımlılık davranışı: PostgreSQL'de müşteri satırı başka bir oturumda kilitliyken yeni karar `3s` sonra `503` döner, log satırında `postgres_error=true` görünür. Kilit kalkınca yeni karar uygulama yeniden başlatılmadan `200` döner. İstemci isteği iptal ederse işlem `1s` içinde biter ve bağlantı havuza döner.

### Düzenli kapanış

`SIGTERM` veya `Ctrl+C` gelince servis yeni bağlantı kabul etmeyi bırakır, devam eden istekleri `10s`'ye kadar bekler, sonra PostgreSQL havuzunu ve Redis istemcisini kapatır. Loglarda sırasıyla `shutdown started` ve `shutdown complete` görünür, süreç `0` koduyla çıkar. İstekler `4s` bütçeyle sınırlı olduğu için bekleme Docker'ın varsayılan `10s` durdurma süresine sığar. Örnek API de aynı şekilde kapanır.

### Karar kayıtlarının ömrü

| Veri | Nerede | Ne kadar kalır |
| --- | --- | --- |
| Hız ön sonucu (`qg:rate:…:decision:…`) | Redis | `24 saat` TTL; bu sürede aynı karar kimliğinin tekrarı hız kapasitesini ikinci kez harcamaz |
| Hız penceresi sayacı (`qg:rate:…:bucket:…`) | Redis | Pencere bitince kendiliğinden silinir |
| Kalıcı karar (`quotagate.decisions`) | PostgreSQL | Elle temizlenene kadar; komut en az `48 saat` tutar |
| Günlük kullanım (`quotagate.daily_usage`) | PostgreSQL | Silinmez; müşteri+işlem+gün başına tek küçük satır |

Kalıcı karar, aynı karar kimliğiyle gelen tekrarın ilk sonucu almasını sağlar. Erken silinirse geç gelen bir tekrar **yeni karar** sayılır ve günlük kotadan ikinci kez hak harcayabilir. Bu yüzden temizlik en az `48 saat` (tekrar penceresinin iki katı) eski kayıtlara dokunur ve bu alt sınır komut satırında değil `store.PruneDecisions` içinde zorunludur.

Temizlik planlayıcı değil, elle çalıştırılan bir komuttur. Varsayılanı yalnızca sayar:

```bash
# Sadece say: 72 saatten eski kaç karar var?
quotagate prune-decisions
# Sınırı değiştir (en az 48h):
quotagate prune-decisions -older-than 96h
# Gerçekten sil:
quotagate prune-decisions -older-than 72h -apply
```

Compose'da aynı komut `api` imajıyla çalışır:

```powershell
docker compose -p quotagate-qg01 --env-file .env.compose run --rm --no-deps api prune-decisions -older-than 72h
```

- Kesme anı PostgreSQL saatinden hesaplanır; uygulama sunucusunun saati kaymış olsa da 48 saat kuralı bozulmaz.
- Silme `1000` satırlık parçalarla yapılır; uzun tek bir kilit tutulmaz. `0004_decision_retention.sql` bu sorgu için `created_at` index'i ekler.
- `48h` altı, `-apply` yazımı yanlış veya fazladan argüman verilirse komut veritabanına hiç bağlanmadan hata verir.
- Komut servisin `5s` `statement_timeout` sınırını kullanmaz; çok satırlı temizlik bu sınıra takılmaz. İptal için `Ctrl+C` yeterlidir, tamamlanmış parçalar silinmiş kalır.
- Log satırı yalnızca `apply`, `older_than`, `cutoff`, `matched` ve `deleted` içerir.

Karar kaydı silinince günlük kullanım değişmez, çünkü `GET /v1/usage` sayıyı `daily_usage` tablosundan okur.

### Testler

Bağımlılık URL'si verilmeden çalışan testler: sessiz PostgreSQL sunucusuna bağlanma `2s` içinde biter, sessiz Redis'e çağrı `1s` içinde biter, istek bütçesi uzun handler'ı keser, kapanış devam eden isteği bitirip yeni bağlantıyı reddeder, temizlik komutu güvensiz argümanları veritabanına gitmeden reddeder.

`QG_TEST_DATABASE_URL` ve `QG_TEST_REDIS_URL` ile çalışan testler: kilitli satırda karar `503` döner ve iptal edilen istek bağlantı tutmaz; bağlamı olmayan yavaş sorgu `statement_timeout` ile `57014` hatası alır; 1, 47, 49 ve 100 saatlik kararlardan yalnızca 49 ve 100 saatlikler silinir, 47 saatlik kararın tekrarı ilk sonucu döner. Kabul sonuçları [STATUS.md](STATUS.md) içindedir.

## QG-11 yük ve doğruluk raporu

`cmd/qgload`, aynı sabit tohumlu yükü önce **ara katmansız** örnek API'ye (taban), sonra **QuotaGate ara katmanlı** örnek API'ye gönderir. İki koşu arasındaki gecikme farkı ara katmanın maliyetidir. QuotaGate koşusunun her yanıtı PostgreSQL kayıtlarıyla karşılaştırılır.

Hedef, ölçümden önce [PLAN.md](PLAN.md#qg-11--yük-ve-doğruluk-raporu) içinde yazıldı ve araç bunu raporun başına koyar: **50 istek/sn, 5 dakika, en çok 20 eşzamanlı istek, p95 ek gecikme < 30 ms**. Hedef tutmasa da rapor yazılır; yanlış veya eksik karar ise koşuyu başarısız yapar (çıkış kodu `1`).

### Yük

| Müşteri | Pay | Günlük limit | Dakika limiti | Beklenen |
| --- | ---: | ---: | ---: | --- |
| 8 açık müşteri | %70 | 1.000.000.000 | 1.000.000 | Hepsine izin |
| 1 günlük müşteri | %15 | 500 | 1.000.000 | Tam 500 izin, gerisi `daily_quota` |
| 1 hız müşterisi | %15 | 1.000.000.000 | 120 | Her dakika penceresinde en çok 120 izin, gerisi `rate_limited` |

- **Sabit tohum (`-seed 11`):** her isteğin hangi müşteriye gideceği tohumdan üretilir. Aynı tohum aynı sırayı verir; iki koşu ve sonraki tekrarlar aynı yükü gönderir.
- **Açık döngü:** istek `i`, başlangıçtan `i/50` saniye sonra gönderilir; yavaş yanıt sonraki istekleri geciktirmez. Gecikme, isteğin **planlanan** gönderim anından yanıtın sonuna kadar ölçülür. Boş işçi bekleyen istek bu bekleyişi de gecikmesine katar (coordinated omission'a karşı).
- **Isınma:** her hedefe önce 10 saniyelik ölçülmeyen yük gider, yalnızca açık müşterilere. Isınma kararları doğruluk kontrolüne dahildir.
- Örnek API'nin taban modu `QUOTAGATE_MIDDLEWARE=off` ile açılır ve yalnızca ölçüm içindir: her `/jobs` isteği handler'ı çalıştırır, başlangıçta uyarı loglanır. Varsayılan `on`.

### Doğruluk kontrolleri

QuotaGate koşusunda (ısınma dahil) şunların hepsi tutmalı:

- Her istek `201` ya da bilinen bir `429` (`daily_quota`, `rate_limited`) aldı; `503` veya bağlantı hatası yok.
- Örnek API handler'ı `201` sayısı kadar çalıştı (`/demo/stats` farkı).
- Her müşteri için PostgreSQL'deki karar kaydı sayısı gönderilen istek sayısına eşit; izin, `daily_quota` ve `rate_limited` kayıtları yanıtlarla aynı; `daily_usage` izin sayısına eşit.
- Açık müşterilerin hepsine izin; günlük müşteriye tam 500 izin.
- Hız müşterisi: Redis dakika pencereleri saate bağlı. İstemcinin gönderme/yanıt zamanlarından, `1s` saat payıyla, kesin bir pencereye düşen istekler için en az `min(120, istek)` izin; dokunulan her pencere için en çok `120` izin.

### Çalıştırma

Compose'da (QG-08 topolojisi ve ayrı taban örnek API'si):

```powershell
docker compose -p quotagate-qg11 --env-file .env.compose -f compose.yaml -f compose.qg08.yaml -f compose.qg11.yaml up --build -d --wait
go run ./cmd/qgload -compose-env .env.compose -note "Docker Desktop Compose, <bilgisayar>" -report reports/qg11-compose.md
docker compose -p quotagate-qg11 --env-file .env.compose -f compose.yaml -f compose.qg08.yaml -f compose.qg11.yaml down
```

Araç yönetici sırrını ve veritabanı parolasını `.env.compose` dosyasından okur, komut satırına yazdırmaz. `-compose-env` yerine `ADMIN_TOKEN` ve `DATABASE_URL` ortam değişkenleri de verilebilir. Varsayılan adresler: QuotaGate `18080`, korumalı örnek API `18081`, taban örnek API `18083`; `-quotagate`, `-guarded`, `-baseline` ile değişir. Koşu yaklaşık 11 dakika sürer. Başlamadan önce araç şunları denetler: QuotaGate hazır mı, korumalı API anahtarsız isteği `401` ile reddediyor mu, taban API anahtarsız isteği `201` ile kabul ediyor mu, koşu UTC gece yarısını geçecek mi (günlük kota sıfırlanırdı).

Araç kendi oluşturduğu 10 test müşterisini koşu sonunda siler (`-keep` ile bırakılır). Politikaları, kararları ve günlük kullanımları da `ON DELETE CASCADE` ile gider. Redis anahtarları kendi süreleriyle silinir. Başka trafik olmayan ortamda çalıştırın: handler sayacı süreç geneli.

Çıktı: Markdown rapor ve yanında aynı adla `.json` (ham sayılar). Raporda commit, Go sürümü, işletim sistemi, CPU, ayarlar, iki koşunun p50/p95/p99/max değerleri, ek gecikme, hedefin sonucu, müşteri bazında karar sayıları ve her kontrolün sonucu bulunur.

### Sınır

Yük üreticisi, iki örnek API, QuotaGate, PostgreSQL ve Redis aynı makinede çalışır; sonuç ara katmanı aynı koşullardaki kendi tabanıyla karşılaştırır. Üretim kapasitesi, HA veya genel p95 SLA iddiası değildir. Tek koşu tek ölçümdür; kuyruk değerleri (p99, max) koşudan koşuya oynar. Ölçüm sonuçları [STATUS.md](STATUS.md) ve [reports/](reports/) içindedir.

## QG-12 yerel Kubernetes

`deploy/k8s/` bir kustomize dizinidir; `kubectl apply -k deploy/k8s` hepsini `quotagate` namespace'ine kurar.

| Kaynak | Ne yapar |
| --- | --- |
| `postgres` StatefulSet + Service | Tek PostgreSQL 17, 1Gi volume; hazır olma kontrolü `pg_isready` |
| `redis` Deployment + Service | `compose.qg08.yaml` ile aynı ayar: disk kalıcılığı yok, `128mb`, `noeviction` |
| `quotagate` Deployment (2 kopya) + Service | `serve-combined`; her kopya başlamadan önce `migrate` init container'ı çalışır |
| `example-api` Deployment + Service | QG-04 örnek API'si, `http://quotagate:8080` Service'ini çağırır |
| `quotagate-secrets` Secret | `deploy/k8s/secrets.env` dosyasından üretilir; dosya Git dışında, repoda yalnızca `secrets.env.example` var |

Bilinçli ayarlar:

- **Readiness `/health/ready`, liveness `/health/live` (QG-09):** Redis veya PostgreSQL kapanınca kopya trafikten çıkar ama yeniden başlatılmaz; bağımlılık gelince kendiliğinden geri döner.
- **`maxUnavailable: 0`, `maxSurge: 1`:** güncellemede önce yeni kopya hazır olur, sonra eskisi durur; iki hazır kopya korunur.
- **Kapanış:** `preStop` 5 saniye bekler, sonra `SIGTERM` gelir ve servis QG-10'daki düzenli kapanışı yapar (en çok 10 sn). `terminationGracePeriodSeconds: 20` ikisini kapsar. Bekleme, durmakta olan pod'un Service'ten çıkarılması her düğüme ulaşmadan yeni isteğin ona gitmesini önlemek içindir.
- **Migration init container'da:** iki kopya aynı anda başlasa da migration çalıştırıcısındaki PostgreSQL advisory lock (`internal/migrate`) migration'ları sırayla tek kez uygular. Uygulama migration bitmeden başlamaz.
- **Bağlantı adresi:** `DATABASE_URL`, Secret'taki parolayı `$(QG_DB_PASSWORD)` ile Kubernetes'e doldurtur; `kubectl describe` parolayı göstermez. Parola URL içine girdiği için yalnızca harf ve rakam kullanın.
- **Güvenlik:** root olmayan kullanıcı (`10001`), salt okunur kök dosya sistemi, ayrıcalık yükseltme kapalı, bütün Linux yetenekleri kaldırılmış, `RuntimeDefault` seccomp. Bellek sınırı QuotaGate `256Mi`, örnek API `128Mi`.

### Kurulum (kind)

Docker Desktop açıkken; kind yoksa `winget install Kubernetes.kind`, `kubectl` Docker Desktop ile gelir:

```powershell
kind create cluster --name quotagate
docker build --target quotagate -t quotagate:local .
docker build --target example-api -t quotagate-example-api:local .
kind load docker-image quotagate:local quotagate-example-api:local --name quotagate
Copy-Item deploy/k8s/secrets.env.example deploy/k8s/secrets.env   # iki değeri değiştirin
kubectl apply -k deploy/k8s
kubectl -n quotagate rollout status deploy/quotagate --timeout=180s
kubectl -n quotagate get pods
```

İlk kurulumda `migrate` init container'ı PostgreSQL hazır olana kadar birkaç kez yeniden denenebilir; bu beklenen durumdur. PostgreSQL ve Redis imajlarını cluster Docker Hub'dan çeker.

Elle denemek için: `kubectl -n quotagate port-forward svc/example-api 18081:8081` ve `kubectl -n quotagate port-forward svc/quotagate 18080:8080`.

Kod değişince imajları yeniden derleyip `kind load` ile yükleyin, sonra `kubectl -n quotagate rollout restart deploy/quotagate deploy/example-api` (etiket hep `:local` olduğu için Kubernetes değişikliği kendisi görmez).

### Duman testi

```powershell
$env:QG_K8S_SMOKE = "1"
go test ./deploy/k8s -run TestKubernetesSmoke -count=1 -v
Remove-Item Env:QG_K8S_SMOKE
```

Test geçerli `kubectl` bağlamını ve `quotagate` namespace'ini kullanır:

1. Tam iki QuotaGate pod'u olana ve ikisi de hazır olana kadar bekler (durmakta olan pod kalmamalı).
2. Port-forward ile admin API'den iki geçici müşteri açar; günlük limiti 2 olan müşteri örnek API'de `201, 201, 429 daily_quota` almalı.
3. Örnek API'nin o anda konuştuğu kopyayı (loglarda kararları gösteren pod) siler ve 10 saniye boyunca her yarım saniyede bir karar ister; 20 kararın hepsi `201` olmalı.
4. Yeni pod hazır olup yine iki kopya olunca 5 karar daha ister; hepsi `201` olmalı.
5. Geçici müşterileri PostgreSQL'den siler.

### Temizlik

```powershell
kubectl delete namespace quotagate     # PostgreSQL volume'u dahil her şeyi siler
kind delete cluster --name quotagate   # cluster'ı tamamen kaldırır
```

### Sınır

Tek düğümlü yerel cluster. Ingress/TLS, PostgreSQL ve Redis için yüksek erişilebilirlik, yedekleme, otomatik ölçekleme, Redis Cluster ve AWS/EKS yok. Kubernetes Secret yalnızca base64 kodludur, şifreli değildir. Kanıt ve ortam ayrıntıları [STATUS.md](STATUS.md) içindedir.

## QG-13 uçtan uca hata demosu

`cmd/qgdemo` tek komutla bütün demoyu yürütür: kendi Compose projesini (`quotagate-qg13`) kurar, iki müşteriyle iki QuotaGate kopyasına sürekli yük gönderir, yük sürerken bir bağımlılığı durdurup yeniden başlatır. Sonra her yanıtı örnek API'lerin handler sayaçları, PostgreSQL ve Redis ile uzlaştırır, rapor yazar ve projeyi hacmiyle birlikte siler.

```powershell
go run ./cmd/qgdemo -outage redis -note "Docker Desktop, <bilgisayar>"
go run ./cmd/qgdemo -outage postgres -note "Docker Desktop, <bilgisayar>"
```

Docker Desktop açık ve `.env.compose` hazır olmalı. Bir koşu ilk derleme hariç yaklaşık iki dakika sürer. Rapor `reports/qg13-<redis|postgres>-<UTC zaman>.md`, yanında aynı adla `.json` (ham sayılar). Bir kontrol bile tutmazsa çıkış kodu `1`.

### Kurulum

| Parça | Ayrıntı |
| --- | --- |
| PostgreSQL, Redis | `compose.yaml` ve `compose.qg08.yaml` ile aynı ayarlar; Redis diske yazmaz |
| İki QuotaGate kopyası | `api` ve `api-2`, `serve-combined` |
| İki örnek API | `example-api` birinci kopyayı, `example-api-2` (`compose.qg13.yaml`) ikinci kopyayı çağırır |
| Portlar | QuotaGate `28080`/`28082`, örnek API `28081`/`28084`, PostgreSQL `25432`, Redis `26379`. Başka bir QuotaGate projesi açıkken de çalışır |

### Senaryo

| Müşteri | Günlük limit | Dakika limiti | Beklenen |
| --- | ---: | ---: | --- |
| basic | 100 | 60 | Her dakika penceresinde en çok 60 hız izni; 100 izinden sonra `daily_quota` |
| pro | 100.000 | 1200 | Limitleri bütün yükünden büyük; hiç reddedilmez |

- **Yük:** saniyede 20 istek, açık döngü (QG-11 `loadtest.Run`). Yarısı bir örnek API'ye, yarısı diğerine gider; müşteriler sırayla gelir. 30 saniye normal, 25 saniye kesinti, sonra 35 saniye (`-before`, `-outage-for`, `-after`).
- **Kesinti:** `docker compose stop redis` ya da `docker compose stop db`; süre dolunca `docker compose up -d --no-deps --wait`.
- **Hazır olma:** araç iki kopyanın `/health/ready` ucunu 250 ms'de bir sorar. İkisi de `503` deyince kesinti fark edilmiş, ikisi de `200` deyince toparlanma bitmiş sayılır. Bütün değişimler rapordaki zaman çizelgesinde.

### Kontroller

- Her yanıt `201`, bilinen bir `429` (`daily_quota`, `rate_limited`) ya da `503`. Handler tam `201` sayısı kadar çalıştı.
- **Kesinti öncesi:** her istek karar aldı.
- **Kesinti sırasında:** yoldaki istekler bittikten sonra (3 saniye) yeniden başlatmaya kadar gönderilen her istek `503` aldı, handler sayacı hiç değişmedi.
- **Toparlanma sonrası:** iki kopya hazır olduktan 3 saniye sonra gönderilen her istek yeniden karar aldı. İki kopya da kesintiden önce ve sonra izin verdi.
- **Müşteri başına PostgreSQL:** karar kaydı, izin, `daily_quota` ve `rate_limited` sayıları yanıtlarla aynı; `daily_usage` izin sayısına eşit; izin günlük limiti geçmedi, `daily_quota` varsa izin tam limitte.
- **Müşteri başına Redis:** her pencere dakika limitinde ya da altında. Redis'in karar kimliği başına 24 saat tuttuğu ön sonuçlar (hız kontrolünü geçen ve geçemeyen) PostgreSQL kayıtlarıyla aynı. Redis kesintisinde bu karşılaştırma yeniden başlatmadan sonrası içindir.
- **pro:** limitleri bütün yükünden büyük olduğu için hiç `429` almadı. basic'in limitleri ona dokunmadı.
- Uygulama konteynerleri yeniden başlamadan bağlantıyı kendisi kurdu (başlama zamanı ve yeniden başlama sayısı aynı). Yönetici sırrı, veritabanı parolası ve iki API anahtarı uygulama loglarında yok.

**Belirsiz `503`:** bağımlılık kapanırken ya da açılırken yolda olan bir istek, QuotaGate Redis'te hak harcadıktan ya da PostgreSQL'e kararı yazdıktan sonra yanıtını kaybetmiş olabilir. Bu yüzden yalnızca o anlarda yoldaki `503`'ler kayıtlarda en çok o kadar fazlalığa izin verir. Tam kesinti sırasındaki `503` iz bırakamaz: Redis kapalıyken PostgreSQL transaction'ı geri alınır, PostgreSQL kapalıyken Redis'e hiç gidilmez.

**Redis yeniden başlayınca:** Redis diske yazmadığı için pencere sayaçları ve ön sonuçlar kaybolur (QG-08 kararı). Durdurma ve başlatma aynı dakikadaysa o dakika limitin iki katına kadar izin verebilir. Rapor bunu sayılarıyla yazar. Yeniden başlatmadan önceki kararlar PostgreSQL'den kontrol edilir: Redis'in açık olduğu her dakika için en çok limit kadar.

### Temizlik ve tekrar

- Başlamadan önce Docker'da `quotagate-qg13` etiketli bir konteyner, hacim, ağ ya da imaj varsa araç çalışmaz ve silme komutunu yazar. Kendi oluşturmadığı hiçbir şeyi kullanmaz ve silmez.
- Sonunda `docker compose ... down -v --remove-orphans --rmi local` çalışır, ardından Docker'a projeye ait bir şey kalıp kalmadığı sorulur. Ctrl+C ve hata durumunda da çalışır. `-keep` projeyi inceleme için bırakır ve silme komutunu yazar.
- `.env.compose` dosyasından yalnızca `QG_ADMIN_TOKEN` ve `QG_DB_PASSWORD` okunur, ekrana ve rapora yazılmaz. Bu dosyadaki port değerleri demo projesinde kullanılmaz.
- `-override` son sıraya bir Compose dosyası daha ekler (örneğin kurumsal proxy için derleme ayarı); rapor bunu yazar.

### Sınır

Tek makine, aynı anda tek bağımlılık kesintisi, planlı `docker compose stop`. Gösterdiği şey bu koşullarda QuotaGate'in davranışı ve toparlanmasıdır; üretim erişilebilirliği, HA ya da kapasite iddiası değildir. Ağ bölünmesi, yavaşlayan (kapanmayan) bağımlılık ve iki bağımlılığın birlikte kapanması denenmedi. Ölçüm sonuçları [STATUS.md](STATUS.md) ve [reports/](reports/) içinde.
