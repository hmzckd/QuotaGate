# QuotaGate: dört sürümlük uygulama planı

Bu plan 14 dikey görevden oluşur. Tek Go modülü, tek karar servisi, küçük Go istemcisi/middleware ve yerel örnek API kullanılır. Başlangıçta PostgreSQL ile kalıcı günlük kullanım gelir; Redis yalnızca v0.2'de kısa dönem hız sınırı gerektiğinde eklenir. REST yeterlidir; gRPC, özel scheduler, event sourcing, çok bölge, Redis Cluster ve üretim HA kapsamda değildir. Her görevi bitirirken `STATUS.md` güncellenir. `DONE` yalnızca ölçülmüş/test edilmiş kabul koşulları için yazılır.

## Sabit ürün ve teknik kararlar

| Karar | Sözleşme ve gerekçe |
| --- | --- |
| Müşteri kimliği ve yetki | QuotaGate'in ürettiği rastgele müşteri anahtarı yalnızca oluşturma veya yenileme yanıtında gösterilir; veritabanında hash'i tutulur. Karar ve kullanım API'sinde müşteri kimliği **anahtardan** çıkarılır; çağıranın verdiği `customer_id` kabul edilmez. Yerel yönetim API'si ayrı bir yönetici sırrıyla müşteri/politika oluşturur, anahtar döndürür veya devre dışı bırakır. Her politika ve sayaç müşteri + işlem ile anahtarlanır. Sırlar loglarda ve metrik etiketlerinde görünmez. Bu yerel HTTP demosu üretim kimlik platformu iddiası taşımaz. |
| İki ayrı sınır | Günlük kota, UTC takvim günü başına **izin verilmiş karar** sayısıdır ve PostgreSQL'de kalır. Hız sınırı, müşteri + işlem için 60 saniyelik sabit pencereye giren ön kabullerin sayısıdır; v0.2'den sonra Redis'te ortak tutulur. Sabit pencere sınırında kısa süreli iki pencere yoğunluğu mümkündür; kayar pencere vaadi yoktur. |
| Başarı değil kabul sayımı | Hak, QuotaGate `allow` kararını kalıcılaştırınca ayrılır; örnek API handler'ının başarı/başarısızlık sonucu kotayı değiştirmez. Bu basit sözleşme uzaktaki sonucun bilinmesini, rezervasyon-iade akışını ve faturalama doğruluğu iddiasını gerektirmez. Kullanıcı başarısız bir handler için de günlük hak harcamış olabilir; arayüz bunu açıkça söyler. |
| Eşzamanlılık | PostgreSQL'de `(customer_id, operation, utc_day)` tekil günlük sayaç satırı koşullu atomik artışla limit altında kalır; karar kaydı ve sayaç aynı transaction'da tamamlanır. `(customer_id, decision_id)` tekildir. Aynı anda aynı kimlikle gelen çağrılardan yalnızca bir transaction yeni hak ayırır. Politika sürümü karar kaydında saklanır. |
| Karar tekrarları | Middleware her gelen örnek API isteği için rastgele bir `decision_id` üretir; yalnızca **aynı QuotaGate çağrısının** taşıma tekrarlarında onu yeniden kullanır. Aynı kimlik + aynı müşteri/işlem için kalıcı sonuç yeniden verilir; farklı içerik `409` olur. En az 24 saatlik tekrar penceresi hedeflenir; PostgreSQL karar kaydı en az 48 saat tutulur. Örnek API'ye yeniden gönderilen ayrı istek yeni kimlik alır ve handler'ın iki kez çalışmasını QuotaGate engellemez. |
| Zaman | Gün sınırı PostgreSQL'in karar transaction'ında aldığı UTC tarihtir: `[00:00, sonraki 00:00)`. Tekrar, ilk kararın kayıtlı gününü ve sonucunu döndürür. Redis kendi `TIME` kaynağıyla 60 saniyelik pencereyi seçer. İki kaynağın sınır anlarında aynı pencereye işaret etmesi beklenmez; ayrı kurallar olarak raporlanır. |
| Birleşik sıra ve kısmi hata | v0.2'de önce PostgreSQL'den varsa kalıcı karar, yoksa güncel müşteri/işlem politikası okunur; ardından Redis'teki atomik hız kontrolü (aynı karar kimliği için ön sonuç tekrar kullanılır), sonra PostgreSQL günlük kota transaction'ı ve nihai karar kaydı gelir. Redis ret sonucu da PostgreSQL'de nihai karar olarak kaydedilir; günlük sayaç artmaz. **Redis ile PostgreSQL arasında transaction yoktur.** Redis izin verip PostgreSQL kota reddederse veya hata alırsa hız kapasitesi harcanmış olabilir; günlük hak yalnızca PostgreSQL `allow` commit ederse harcanır. Redis ön kararının 24 saatlik TTL'si, PostgreSQL'e yazılmadan kaybolan yanıtın aynı kimlikle yeniden denenmesini destekler; Redis veri kaybından sonra tamamlanmamış ön karar için aynı sonuç garantisi yoktur. |
| Kesinti davranışı | Redis veya PostgreSQL erişilemiyorsa yeni istek için fail-closed `503`; handler çalışmaz. Redis kota reddi ve PostgreSQL kota reddi uygulamada `429` olur. PostgreSQL commit edip yanıt kaybolursa middleware `503` verebilir ve handler çalışmaz, fakat hak ayrılmış olabilir; aynı `decision_id` ile tekrar kalıcı sonucu açığa çıkarır. Sağlık/readiness kesintiyi gösterir; toparlanınca servis yeniden karar alır. |
| Kaynak sınırları | HTTP ve DB çağrıları bağlam/deadline ile sınırlanır, bağlantı havuzları sınırlıdır; uygulama kapanışında dinleyici ve bağlantılar düzenli kapatılır. V0.3'te timeout, iptal ve yeniden bağlanma testleri tamamlanır. |

**API çizgisi:** `POST /v1/decisions` `{decision_id, operation}` alır; müşteri anahtarı header'dan gelir. Tanımlı politika için `200` ve `{allowed, reason, utc_day, daily_used, daily_limit, policy_version}` döner; `reason` en az `allowed`, `rate_limited`, `daily_quota` değerlerini ayırır. Tanımsız işlem politikası `403`, kimlik hatası `401`, farklı içerikle aynı kimlik `409`, bağımlılık nedeniyle sonuç alınamaması `503` olur. Middleware kota/hız reddi için `429`, karar alınamazsa `503` döner; hiçbir ret halinde handler çalışmaz. `GET /v1/usage?operation=...` yalnızca anahtar sahibinin PostgreSQL günlük sayacını verir; Redis ön kabul sayısını **günlük kullanım** diye göstermez. Yönetim uçları yerel demo için ayrı yetkilendirilir. Politika değiştirilirse yeni karar yeni sürümü kullanır; eski karar tekrarı kayıtlı sonucu verir.

## v0.1 — kalıcı günlük kota, tek servis

**Çalışan demo:** Yönetici iki müşteri ve `job.create` politikası tanımlar; her biri kendi anahtarıyla örnek API'yi çağırır. Günlük limit dolunca handler çalışmaz. Kullanım sorgusu müşteri verilerini ayırır. Redis bu sürümde yoktur.

### QG-01 — Go servis iskeleti ve yerel PostgreSQL temeli

- **Amaç:** Yeni `QuotaGate/` klasöründe tek Go modülünü, HTTP servisini, yapılandırmayı, migration altyapısını ve PostgreSQL'li Compose çalıştırmasını kurmak.
- **Kısa bağlam:** Bu planın sabit kararları, `README.md`, `STATUS.md`; uygulanan temel yollar `go.mod`, `cmd/quotagate`, `internal/config`, `internal/httpapi`, `migrations`, `compose.yaml`.
- **Kapsam sınırı:** Yalnızca `/health/live` ve veritabanı bağlantısını kontrol eden `/health/ready`, kapanış ve temel konfigürasyon; müşteri, Redis, kota, middleware ve Kubernetes yok. Sırlar örnek env dosyasında sahte değer olmalı.
- **Kabul:** `go test ./...` geçer; Compose ile servis + PostgreSQL ayağa kalkar, migration tekrar çalıştırılabilir, readiness DB kesilince başarısız ve geri gelince başarılı olur; durdurma bağlantıları kapatır. Komutlar ve gözlenen çıktılar `STATUS.md`'ye yazılır.
- **Doğrulama:** Go test; `docker compose up --build -d --wait`, health çağrıları, PostgreSQL'i geçici durdurup başlatma, ardından Compose log/cleanup. Mevcut HookRelay Compose kaynaklarını kullanma veya silme.

### QG-02 — müşteri anahtarı, politika ve izolasyon

- **Amaç:** Yerel yönetici akışında müşteri oluşturma/anahtar yenileme veya devre dışı bırakma, işlem politikası yazma ve yetkili müşteri tanıma.
- **Kısa bağlam:** QG-01 HTTP, yapılandırma ve migration; anahtar/politika sözleşmesi yukarıda. Dokunulacak alanlar: yeni müşteri/politika tabloları, admin/customer HTTP katmanı ve depolama testleri.
- **Kapsam sınırı:** Genel kullanıcı yönetimi, OAuth/SSO, self-service portal ve çok rollü IAM yok. Yönetici sırrı yalnızca yerel demo için; anahtar ham biçimde saklanmaz.
- **Kabul:** İki müşteri oluşturulup farklı limit alır; A'nın anahtarıyla B'nin politikasına erişilemez; iptal edilen anahtar `401` olur; aynı müşteri/işlem politikası sürümlenir; anahtar ve yönetici sırrı loglarda görünmez. Kullanım uç noktası QG-03'te eklenir ve aynı müşteri izolasyonu orada doğrulanır.
- **Doğrulama:** Handler/depo testleri, gerçek PostgreSQL entegrasyonu ve iki müşteriyle yetki/izolasyon HTTP denemeleri.

### QG-03 — atomik günlük karar ve kullanım

- **Amaç:** PostgreSQL transaction'ıyla günlük hakkı ayırmak, nihai kararı kaydetmek ve müşteri kullanımını sorgulatmak.
- **Kısa bağlam:** QG-02 müşteri/politika şeması; `POST /v1/decisions`, `GET /v1/usage`, tekrar ve UTC kararları. Yeni karar/counter migration'ı ve Go işlem katmanı.
- **Kapsam sınırı:** Redis ve kısa dönem hız kontrolü yok; handler başarısına göre iade yok. Aynı karar kimliğiyle farklı operation `409` olmalı.
- **Kabul:** Limit N iken eşzamanlı benzersiz çağrılardan en çok N tanesi `allow` olur; tekrar aynı sonucu verir ve sayaç artmaz; yanıt kaybı sonrası aynı kimlik kayıtlı sonucu bulur; UTC gece yarısı sınırı kontrollü saat testiyle doğrulanır; kullanım yalnızca kendi müşterisine aittir.
- **Doğrulama:** Gerçek PostgreSQL ile transaction/eşzamanlılık testleri (`go test -race ./...` dahil); tekrarlı istek, politika değişimi ve gün devri testleri.

### QG-04 — dar Go middleware ve örnek API

- **Amaç:** QuotaGate istemcisini küçük bir Go middleware'e bağlayıp `POST /jobs` örneğini çalıştırmak.
- **Kısa bağlam:** QG-03 karar/ret/hata sözleşmesi; yeni `client` ve `examples/api` yolları. İstek akışı `README.md`'de.
- **Kapsam sınırı:** Örnek API işi gerçek işe/ücretli API'ye bağlanmaz; proxy, genel routing ve otomatik idempotent handler yok. Bu sürümde tek QuotaGate çağrısı yeterli.
- **Kabul:** İzin handler'ı bir kez çalıştırır; günlük ret `429`, QuotaGate ulaşılamaması `503` verir ve ikisinde handler çalışmaz; her gelen API isteği yeni karar kimliği alır; iki müşteri farklı limitlerle canlı gösterilir.
- **Doğrulama:** Middleware HTTP testleri ve Compose ile iki müşteri uçtan uca duman testi. **v0.1 DONE:** bu demo ve QG-01–04 kabulü geçer.

## v0.2 — paylaşılan hız sınırı ve kısmi başarısızlık

**Çalışan demo:** İki QuotaGate kopyası aynı Redis/PostgreSQL'e bağlanır; kısa pencere limiti kopyalar arasında ortaktır. Günlük sayaç yalnızca nihai izinleri gösterir; Redis kesintisinde yeni istekler kapanır.

### QG-05 — Redis'te ortak kısa pencere

- **Amaç:** Müşteri + işlem için 60 saniyelik sabit pencere hız sınırını Redis'te atomik uygulamak.
- **Kısa bağlam:** QG-02 politikası ve QG-03 kararı; Redis servis bağlantısı, küçük Lua script'i ve Compose eklemesi. PostgreSQL günlük kota ayrı kalır.
- **Kapsam sınırı:** Kayar pencere, Redis Cluster ve Redis'i kalıcı kullanım kaynağı yapma yok.
- **Kabul:** İki servis kopyası üzerinden eşzamanlı çağrılar toplam pencere limitini aşmaz; müşteri/işlem anahtarları ayrıdır; Redis `TIME` ile pencere ve `TTL` açıkça test edilir; script aynı karar kimliğinin ön sonucunu tekrar verir.
- **Doğrulama:** Gerçek Redis entegrasyon testi, pencere sınırı ve iki kopyalı Compose denemesi; yarış/limit için `go test -race ./...`.

### QG-06 — birleşik karar sırası ve kalıcı sonuç

- **Amaç:** PostgreSQL'den kayıtlı sonucu okuma → Redis ön kararı → PostgreSQL günlük transaction/nihai karar sırasını tamamlamak.
- **Kısa bağlam:** QG-03 karar transaction'ı, QG-05 Redis script'i ve yukarıdaki kısmi hata tablosu. API'de `rate_limited` ile `daily_quota` ayrı nedenlerdir.
- **Kapsam sınırı:** İki depoyu tek atomik işlem gibi sunma, rate ön kabulünü geri alma, handler sonucuna göre hak iadesi yok.
- **Kabul:** Redis retleri ve günlük retler ayrı raporlanır; günlük ret veya DB arızası öncesinde tüketilen Redis kapasitesi geri gelmez; DB'ye commit edilmiş sonucun tekrarı Redis'e gitmeden döner; aynı kimlikte yarış yalnızca bir hak ayırır; farklı içerik `409` olur.
- **Doğrulama:** Gerçek iki bağımlılıkla hata noktası enjeksiyonu ve transaction/eşzamanlılık testleri; PostgreSQL sayaçlarıyla karar kayıtlarını karşılaştırma.

### QG-07 — istemci tekrarı ve handler sınırı

- **Amaç:** QuotaGate yanıt kaybı veya geçici bağlantı hatasında, aynı gelen API isteğinin karar kimliğiyle sınırlı tekrar yapan istemci/middleware kurmak.
- **Kısa bağlam:** QG-04 istemci/örnek API, QG-06 kalıcı sonuç; idempotency ve `503` davranışı yukarıda.
- **Kapsam sınırı:** Örnek API işini tekrar gönderen istemcileri tek iş sayma yok; QuotaGate dışında exactly-once etki iddiası yok.
- **Kabul:** Aynı inbound istek içindeki tekrar aynı `decision_id` kullanır; timeout bütçesi dolunca handler çalışmaz ve `503` döner; commit sonrası yanıt kaybında tekrar kayıtlı sonucu bulur; ayrı inbound istek yeni kimlik/hak alır.
- **Doğrulama:** HTTP fault-injection testi, gerçek DB ile drop-after-commit senaryosu ve yan etki sayacını gösteren örnek API testi.

### QG-08 — iki kopyalı kesinti ve toparlanma

- **Amaç:** Compose'da iki karar servisi kopyasının aynı limitleri paylaşmasını ve bağımlılık kesintisinden toparlanmasını göstermek.
- **Kısa bağlam:** QG-05–07 protokolü; ayrı Compose proje adı ve hacim kullan. Kesinti sınırları yukarıda.
- **Kapsam sınırı:** Üretim HA, otomatik failover veya Redis veri kaybında rate devamlılığı vaadi yok.
- **Kabul:** Redis kapalıyken yeni karar `503` ve kullanım değişmez; PostgreSQL kapalıyken `503`, varsa Redis ön kapasitesi tüketilmiş olabilir; geri geldiklerinde yeni karar alınır; iki kopya toplam günlük ve rate limitini aşmaz.
- **Doğrulama:** Gerçek Compose kesinti/başlatma testi, iki kopya eşzamanlı yükü, sayaç/karar sorgusu. **v0.2 DONE:** paylaşılan limit ve kesinti demosu geçer.

## v0.3 — işletim kanıtı ve ölçüm

**Çalışan demo:** Yerel yük altında doğru limit sonuçları, p95 ek gecikme raporu, sağlık/ölçüm ve kontrollü kesinti sonrası toparlanma kaydı sunulur.

### QG-09 — gözlemlenebilir kararlar

- **Amaç:** Nedene göre karar sayacı, bağımlılık hatası, istek süresi ve readiness için sade metrik/log üretmek.
- **Kısa bağlam:** QG-06 karar nedenleri, QG-08 kesinti akışı; mevcut HTTP ve dependency katmanına ölçüm ekle.
- **Kapsam sınırı:** Müşteri anahtarı veya yüksek kardinaliteli müşteri/karar kimliği metrik etiketi olmaz; tam tracing platformu yok.
- **Kabul:** Allow/rate/daily/503 ayrı gözlenir, p95 için histogram vardır; loglar correlation ID taşır ama sır içermez; readiness bağımlılıkları doğru yansıtır.
- **Doğrulama:** HTTP/metrik testleri, gerçek kesinti sırasında metrik ve log incelemesi.

### QG-10 — timeout, kapanış ve veri ömrü

- **Amaç:** Tüm dış çağrılara son tarih, havuz sınırı, iptal, düzenli kapanış ve karar kaydı saklama prosedürü eklemek.
- **Kısa bağlam:** QG-01 servis yaşam döngüsü, QG-07 tekrar, QG-09 sağlık. En az 24 saat tekrar, en az 48 saat kayıt saklama sözleşmesini koru.
- **Kapsam sınırı:** Özel scheduler, otomatik yedekleme ürünü veya production DR yok; eski kayıtların temizliği güvenli, açık bir yerel bakım komutu/prosedürü olabilir.
- **Kabul:** Askıda bağımlılık çağrısı deadline ile biter; iptal edilmiş istek bağlantıyı tutmaz; kapanış tamamlanır; 48 saatten genç kararlar temizlenmez; saklama/temizlik etkisi belgelenir.
- **Doğrulama:** İptal/deadline ve gerçek bağlantı kesintisi testleri; bakım komutunu izole veritabanında dry-run/uygulama ile karşılaştırma.

### QG-11 — yük ve doğruluk raporu

- **Amaç:** Tekrarlanabilir yerel yük aracı ve başlangıç performans raporu üretmek.
- **Kısa bağlam:** QG-04 örnek API, QG-08 iki kopya, QG-09 metrikler; benchmark koşulları sürümle kaydedilir.
- **Kapsam sınırı:** Yerel sonuçlardan üretim kapasitesi, HA veya genel p95 SLA çıkarma yok.
- **Kabul:** Aynı donanım/Compose ayarında middleware kapalı taban ile açık deneme karşılaştırılır; örnek hedef **50 istek/sn, 5 dk, eşzamanlılık 20 ve p95 ek gecikme <30 ms** olarak ölçümden önce yazılır. Sonuç hedefi tutsa da tutmasa da p50/p95 ek süre, hata oranı, karar nedenleri, limit doğruluğu ve ortam raporlanır; yanlış veya eksik karar kabul edilmez.
- **Doğrulama:** Sabit tohumlu yük, sonuçların PostgreSQL karar/sayaçlarıyla uzlaştırılması ve `go test -race ./...`. **v0.3 DONE:** tekrarlanabilir rapor ve doğruluk kanıtı vardır; performans hedefi gerçek ölçüm olarak ayrıca işaretlenir.

## v1.0 — yerel dağıtım ve son kabul

**Çalışan demo:** Farklı politikalı iki müşteri; iki Go karar servisi kopyası; eşzamanlı yük; bir Redis veya PostgreSQL kesintisi ve toparlanması; kota/rate sonuçlarının ve ölçüm koşullarının raporu. Compose ana doğrulama ortamıdır, yerel Kubernetes ikinci dağıtım demosudur.

### QG-12 — yerel Kubernetes temel dağıtımı

- **Amaç:** Mevcut imajları yerel Kubernetes'te temel Deployment/Service/Config/Secret/health ile çalıştırmak ve iki karar servisi kopyası göstermek.
- **Kısa bağlam:** QG-08 Compose topolojisi, QG-09 readiness, QG-10 kapanış. Yerel kind veya Minikube seçimi uygulama anında mevcut araçlara göre yapılır.
- **Kapsam sınırı:** AWS/EKS, ingress/TLS otomasyonu, production HA, yedekleme, autoscaling ve Redis Cluster yok. Gerçek sırlar repoya konmaz.
- **Kabul:** Yerel cluster'da iki servis kopyası hazır olur, örnek API allow/deny verir, bir pod yeniden başlatılınca yeni karar alınır; kurulum/temizleme komutları belgelenir.
- **Doğrulama:** Temiz cluster veya ayrı namespace üzerinde duman testi; Pod/Service/readiness ve karar sonuçları kaydedilir.

### QG-13 — son uçtan uca hata demosu

- **Amaç:** Tek komut dizisiyle iki müşteri, iki kopya, eşzamanlı yük, bir bağımlılık kesintisi ve toparlanmasını gösteren yerel demo hazırlamak.
- **Kısa bağlam:** QG-08 kesinti, QG-11 yük/ölçüm ve QG-12 dağıtım; Compose varsayılan demo yolu.
- **Kapsam sınırı:** Demo kendi izole Compose proje/hacmini kullanır; başka projelerin verisine dokunmaz. Çalışır yük testi production dayanıklılığı iddiası değildir.
- **Kabul:** Her müşteri için allow/rate/daily/503 sayıları, PostgreSQL günlük kullanım ve Redis pencere davranışı tutarlı raporlanır; kesintide handler çalışmaz; toparlanma sonrası yeni isteğe yanıt vardır; demo tekrarlanabilir ve temizlenebilir.
- **Doğrulama:** Baştan sona demo iki kez, sonuç uzlaştırması, kaynak temizliği ve kalan kaynak kontrolü.

### QG-14 — v1.0 kabul ve portföy sunumu

- **Amaç:** API sözleşmesini, mimari sınırları, hata semantiğini, yerel kurulum/demoyu ve gerçek ölçüm sonuçlarını kısa README/raporla sunmak.
- **Kısa bağlam:** QG-01–13 kanıtları, bu kararlar ve `STATUS.md`; tamamlanmayanları açıkça ayır.
- **Kapsam sınırı:** GitHub açma, commit/push/tag, ücretli bulut kurulumu ve yapılmamış AWS/Kubernetes/HA iddiası yok.
- **Kabul:** Temiz yerel kurulum ve demo yönergeleri başkası tarafından izlenebilir; v0.1/v0.2/v0.3/v1.0 bitiş kanıtları, ölçülmüş p95 ve doğruluk, bilinen sınırlar, gelecek isteğe bağlı AWS çalışması ayrı belirtilir; `STATUS.md` gerçek bitiş durumunu gösterir.
- **Doğrulama:** Belgelerdeki komutlarla sıfırdan yerel kabul koşusu ve rapor/veri tutarlılığı incelemesi. **v1.0 DONE:** QG-12–14 ve son demo kabulü geçer.

## Uygulama sırasında korunacak kanıt sınırı

PostgreSQL günlük sayaç ve nihai karar için kaynak gerçektir. Redis kısa dönem hız sınırı için kaynak gerçektir; iki depo arasında atomiklik yoktur. Kesinti veya yanıt kaybı `503` üretirken Redis kapasitesi ya da PostgreSQL'de ayrılmış günlük hak harcanmış olabilir. Başarı sayımı, ödeme doğruluğu, exactly-once dış etki ve üretim kapasitesi iddiaları bu plandan çıkarılamaz.

Teknik dayanak: [PostgreSQL `INSERT ... ON CONFLICT` ve `RETURNING`](https://www.postgresql.org/docs/current/sql-insert.html), [PostgreSQL zaman işlevleri](https://www.postgresql.org/docs/current/functions-datetime.html), [Redis Lua script atomikliği ve script cache sınırları](https://redis.io/docs/latest/develop/programmability/eval-intro/). Bu kaynaklar depo içi koşullu artış ve Redis içi atomiklik içindir; depolar arası atomiklik çıkarımı değildir.
